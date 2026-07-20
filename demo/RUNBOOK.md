# NICo (NVIDIA Infra Controller) — Local Demo Runbook

Full working demo of https://docs.nvidia.com/infra-controller on one machine:
no real hardware. NICo control plane runs on a kind cluster; machine-a-tron
simulates 10 hosts x 2 BlueField DPUs + 2 NVIDIA switches (mock Redfish BMCs);
three FRR containers run an EVPN symmetric-IRB fabric (ToR + two DPU/HBN
nodes) carrying per-VPC VRFs over VXLAN — the data plane NICo programs on
real BlueFields.

Conceptual background (what NICo is for, Crusoe-stack alignment, storage,
OVN/DOCA-OVS, fabric partitioning, expansion roadmap): [notes/](notes/README.md).

## Topology

```ini
 mac (colima VM, docker)
 ├── kind cluster "nico"
 │    ├── cert-manager / Postgres / Vault / local PKI  (bootstrap-prereqs.sh)
 │    └── ns nico-system
 │         ├── nico-api          core gRPC API ("Forge"), port 1079
 │         ├── nico-bmc-proxy    Redfish proxy
 │         ├── nico-ntp x3
 │         └── machine-a-tron    mock fleet: 10 hosts, 20 DPUs, 2 NVOS switches
 └── docker network nico-fabric (172.30.0.0/24, underlay)
      ├── tor-leaf-01  FRR, AS 65100, .2      underlay + EVPN transit
      ├── dpu-hbn-01   FRR, AS 65201, .3      VTEP 10.180.62.1
      │                                        vrf-blue  (L3VNI 2024549): 10.10.10.1/24
      │                                        vrf-green (L3VNI 2024536): —
      └── dpu-hbn-02   FRR, AS 65202, .4      VTEP 10.180.62.2
                                               vrf-blue  : —
                                               vrf-green : 10.20.20.1/24
```

## One-time setup (already done on this machine)

```bash
brew install kind devspace grpcurl helm
colima start --cpu 8 --memory 16
git clone https://github.com/NVIDIA/infra-controller.git ~/infra-controller
cd ~/infra-controller
kind create cluster --name nico
./dev/deployment/devspace/bootstrap-prereqs.sh   # cert-manager, postgres, vault, issuer
devspace deploy -n nico-system                   # builds 3 images (Rust, ~40 min first time)
cd demo/frr && docker compose up -d              # ToR + DPU FRR pair
```

Local changes made to the repo (all in git working tree):

- `devspace.yaml`: `kind load docker-image --name "${CONTEXT#kind-}"` (upstream
   hook assumes cluster name "kind")
- `dev/deployment/devspace/values.base.yaml`:
   - `vpc_peering_policy = "mixed"` (peering is disabled by default)
   - `[site_explorer] create_switches = true` (defaults to false, unlike
      `create_machines` — without it explored switches are never ingested)

- `dev/deployment/devspace/machine-a-tron.yaml`:
   - added `[machines.switches]` section — 2 mock NVOS switches
      (`hw_type = "nvidia_switch_nd5200_ld"`)
   - `host_bmc_password = "vault-password"` / `dpu_bmc_password =
      "vault-password"` — mock BMCs must accept the Vault site-wide root
      credential (`secrets/machines/bmc/site/root`, seeded by
      bootstrap-prereqs.sh) or every Redfish call 401s and machines stall
      at WAITINGFORPLATFORMPOWERCYCLE

## One-time seeding after every fresh DB (REQUIRED)

Site-explorer can't log into the mock BMCs until NICo knows the factory
defaults (bmc-mock constants in `crates/bmc-mock/src/lib.rs`):

```bash
./demo/nico-cli.sh credential add-dpu-factory-default --username root --password 0penBmc
./demo/nico-cli.sh credential add-host-factory-default --username root --password factory_password --vendor dell
./demo/nico-cli.sh credential add-host-factory-default --username root --password factory_password --vendor nvidia
```

If site-explorer already tried (and failed) before you seeded credentials, it
marks endpoints "AvoidLockout" and stops probing them. Clear with:

```bash
kubectl exec -n nico-system deploy/nico-api -- bash -c \
  'CLI="/opt/carbide/nico-admin-cli --root-ca-path=/var/run/secrets/spiffe.io/ca.crt \
   --client-cert-path=/var/run/secrets/spiffe.io/tls.crt \
   --client-key-path=/var/run/secrets/spiffe.io/tls.key -a https://localhost:1079"; \
   for i in $(seq 2 33); do $CLI site-explorer refresh 192.168.192.$i; done'
```

## Demo 1 — Day-0: zero-touch discovery and ingestion

Mock hosts/DPUs DHCP through a simulated relay, PXE, get ingested, and walk
the lifecycle state machine to Ready. Watch it live:

```bash
# CLI wrapper (execs nico-admin-cli inside the nico-api pod with its mTLS certs)
./demo/nico-cli.sh machine show            # states: DISCOVERING -> DPUINIT -> ... -> READY
./demo/nico-cli.sh dpu show
kubectl logs -n nico-system deploy/machine-a-tron -f   # DHCP/FSM activity
```

## Demo 2 — Switch simulation and DPU fleet health

The two mock NVOS switches self-register, are explored via their mock BMCs
(chassis `MGX_NVSwitch_0/1`), and get ingested as Switch objects:

```bash
./demo/nico-cli.sh expected-switch show    # serials MT0200000000xx, linked to sw100... IDs
./demo/nico-cli.sh switch show             # switch objects, state=created
```

The 20 mock DPUs run a simulated forge-dpu-agent (agent-control +
network-observation loops against the API):

```bash
./demo/nico-cli.sh dpu status              # BlueField-3 DPU | Ready | Healthy | Up to date
./demo/nico-cli.sh dpu network --help      # per-DPU networking info
```

## Demo 3 — VPCs and VPC peering

VPC create is a tenant-facing gRPC (`forge.Forge`); peering is operator CLI.

```bash
# API access from the mac: port-forward + certs from the pod.
# The port-forward dies whenever its shell closes or the nico-api pod
# restarts ("Failed to dial ... EOF" = it's gone; just re-run it).
pkill -f "port-forward -n nico-system" 2>/dev/null
kubectl port-forward -n nico-system deploy/nico-api 1079:1079 &
mkdir -p /tmp/nico-certs
kubectl exec -n nico-system deploy/nico-api -- \
  tar cf - -C /var/run/secrets/spiffe.io/..data . | tar xf - -C /tmp/nico-certs

GRPC() { grpcurl -insecure --cert /tmp/nico-certs/tls.crt --key /tmp/nico-certs/tls.key "$@"; }

# Two VPCs for tenant "demo-org" (VNI auto-allocated from the site pool)
GRPC -d '{"metadata":{"name":"vpc-blue"},"tenantOrganizationId":"demo-org"}'  localhost:1079 forge.Forge.CreateVpc
GRPC -d '{"metadata":{"name":"vpc-green"},"tenantOrganizationId":"demo-org"}' localhost:1079 forge.Forge.CreateVpc

# Peer them (IDs from the CreateVpc output)
./demo/nico-cli.sh vpc show
./demo/nico-cli.sh vpc-peering create <VPC_BLUE_ID> <VPC_GREEN_ID>
./demo/nico-cli.sh vpc-peering show
```

Narrative: in production the peering object drives per-VPC VRFs + EVPN route
leaking on every DPU (EthernetVirtualizer/FNN); NICo also supports Flat VPCs
for zero-DPU hosts where peering is bookkeeping + operator fabric config.

## Demo 4 — Live ToR <-> DPU BGP sessions (FRR)

Topology: two "DPUs" (one hosting each VPC's instance), both eBGP+BFD to the
ToR leaf. The underlay carries only VTEP loopbacks and fabric prefixes —
tenant subnets live inside the per-VPC VRFs (Demo 5).

```sh
 dpu-hbn-01 AS65201 VTEP 10.180.62.1 --\
                                        tor-leaf-01 AS65100
 dpu-hbn-02 AS65202 VTEP 10.180.62.2 --/
```

```bash
cd demo/frr && docker compose up -d
docker exec tor-leaf-01 vtysh -c 'show bgp summary'      # ipv4 + evpn AFs Established, BFD up
docker exec tor-leaf-01 vtysh -c 'show ip bgp'           # DPU VTEP loopbacks (no tenant routes)
docker exec tor-leaf-01 vtysh -c 'show bfd peers brief'
```

Narrative: each dpu-hbn plays the HBN/FRR side of a BF3 DPU. This is the
session NICo's forge-dpu-agent provisions via NVUE and health-monitors (see
`crates/agent/src/hbn.rs`, `health/bgp.rs`). Kill one to show BFD tearing the
session down in <1s: `docker stop dpu-hbn-01`.

## Demo 5 — VPC peering over EVPN symmetric IRB (the real HBN architecture)

The FRR fabric now runs the exact architecture NICo programs on BlueFields:

```py
 dpu-hbn-01  AS65201  VTEP 10.180.62.1   vrf-blue(L3VNI 2024549): inst 10.10.10.1/24
                                          vrf-green(L3VNI 2024536): empty
        \                                                             /
         eBGP ipv4 + l2vpn evpn (next-hop unchanged) -- tor-leaf-01 AS65100
        /                                                             \
 dpu-hbn-02  AS65202  VTEP 10.180.62.2   vrf-blue: empty
                                          vrf-green: inst 10.20.20.1/24
```

- Each DPU carries a kernel VRF per VPC (table id = the VPC's NICo VNI) with
   an L3VNI: bridge SVI + VXLAN device sourced from the VTEP loopback.
- Tenant routes travel as EVPN type-5 over VXLAN; the ToR is pure underlay +
   EVPN transit and never sees tenant prefixes in its IPv4 table.
- All SVIs on a DPU share one system MAC (as HBN does) so the DPU advertises
   a single router-MAC.

```bash
# EVPN control plane
docker exec tor-leaf-01 vtysh -c 'show bgp l2vpn evpn summary'
docker exec dpu-hbn-02 vtysh -c 'show bgp l2vpn evpn'        # type-5 routes
docker exec dpu-hbn-02 vtysh -c 'show evpn rmac vni all'
docker exec dpu-hbn-02 vtysh -c 'show ip route vrf vrf-blue' # B>* via VTEP, br-vrf-blue onlink

# 1. Isolated by default — VRFs, not filters
docker exec dpu-hbn-02 ip vrf exec vrf-green ping -c2 -I 10.20.20.1 10.10.10.1  # loss

# 2. Control plane: the peering object
../nico-cli.sh vpc-peering create <VPC_BLUE_ID> <VPC_GREEN_ID>

# 3. Data plane: leak routes between the VRFs on EVERY DPU
#    (= what forge-dpu-agent programs on each BlueField)
./vpc-peering-create.sh

# 4. Cross-VPC traffic flows through the VXLAN fabric
docker exec dpu-hbn-02 ip vrf exec vrf-green ping -c3 -I 10.20.20.1 10.10.10.1  # replies
docker exec dpu-hbn-02 ip -s link show vni2024549                    # TX counters increment

# 5. Unpeer — isolation restored on all DPUs
./vpc-peering-delete.sh
```

Prerequisite (already done on this machine): the colima VM image ships no
`vrf`/`vxlan` kernel modules and its apt index moves past the running kernel,
so the matching package was fetched from Launchpad:

```bash
colima ssh -- sh -c 'curl -sL -o /tmp/lme.deb https://launchpad.net/ubuntu/+archive/primary/+files/linux-modules-extra-$(uname -r)_6.8.0-64.67_arm64.deb \
  && sudo apt-get install -y wireless-regdb && sudo dpkg -i /tmp/lme.deb \
  && sudo modprobe vrf && sudo modprobe vxlan'
```

Debugging note: if cross-VPC pings fail while routes look right, check that
the encap router-MAC matches the far SVI (`ip neigh show dev br-vrf-blue` vs
the peer's bridge MAC). Distinct per-VRF SVI MACs break symmetric IRB — hence
the shared system MAC in init-evpn.sh.

## Demo 6 — Cloud VPC Interconnect (the L3-architecture version)

Simulates Crusoe's VPC Interconnect product as it would look on an EVPN/L3
fabric: the border leaf (tor-leaf-01) holds the customer VRF and peers with
the external cloud inside it. No ovnbgp/OpenFlow glue, no per-interconnect
VLAN plumbing — the interconnect is one more BGP session in a VRF.

```ini
 dpu-hbn-01 (vrf-blue: 10.10.10.1/24) ──┐
                                        ├─ EVPN fabric ── tor-leaf-01 (border leaf,
 dpu-hbn-02 (vrf-green: 10.20.20.1/24) ─┘                  VTEP 10.180.62.3,
                                                            vrf-blue holds eth1)
                                                                 │ 172.31.0.0/24
                                                                 │ (dedicated port)
                                                            gcp-router AS 16550
                                                            "GCP VPC" 10.128.0.0/20
```

AS 16550 = the real GCP Partner Interconnect ASN; 10.128.0.0/20 matches the
SSI production deployment.

```bash
cd demo/frr
./interconnect-up.sh                # = the entire interconnect enablement
docker exec tor-leaf-01 vtysh -c 'show bgp vrf vrf-blue summary'   # GCP session Established
docker exec dpu-hbn-01 vtysh -c 'show ip route vrf vrf-blue'       # 10.128.0.0/20 as type-5
docker exec dpu-hbn-01 ip vrf exec vrf-blue ping -c3 -I 10.10.10.1 10.128.0.1   # ~0.8ms
docker exec gcp-router ping -c2 -I 10.128.0.1 10.10.10.1           # return path
./interconnect-down.sh              # session shut -> routes withdrawn in seconds
```

Talking points vs the MVP (OVNGW/ovnbgp/OpenFlow):

- Route exchange is plain BGP->EVPN type-5; no BGP->OpenFlow translation layer.
- Isolation is the VRF/VNI itself; no per-interconnect VLAN on the fabric
   (only the physical handoff keeps one).
- With VPC peering active, vpc-green transitively reaches GCP through
   vrf-blue's leak — real deployments would scope this with route-targets;
   good discussion point on peering vs interconnect policy interaction.
- MVP limitations dissolve: multi-VPC per site = more VRFs; no supernet
   restriction (exact prefixes, no static steering routes).

## Demo 7 — The tenant REST API surface (what a customer actually drives)

Demos 1-6 use the operator/Forge gRPC on the `nico` cluster. This one uses the
*tenant-facing* REST API on the separate `nico-rest-local` cluster: the same
authenticated surface a customer gets (org-scoped `/v2/org/{org}/nico/...`,
Keycloak JWT, RBAC), covering compute, networking, fabric partitions, keys,
and images. No BMC, no hypervisor.

```bash
# Needs the nico-rest-local stack up + tunnels on 8082 (Keycloak) and 8388 (API).
cd demo
./tenant-api-tour.sh            # prints the full tenant catalog, then proves a
                                # slice live: mints a real TENANT_ADMIN JWT and
                                # runs authenticated GETs (all HTTP 200)
./tenant-api-tour.sh --catalog  # catalog only (no stack needed — safe fallback)
```

Auth recipe (local realm `nico-dev`): client `nico-api` / secret
`nico-local-secret` (direct grant), user `testuser` password `demo`
(`kcadm set-password` if unset), org `test-org` = TENANT_ADMIN.
Gotcha: if GETs 401 right after a Keycloak restart, the API cached the old
JWKS — `kubectl -n nico-rest rollout restart deploy/nico-rest-api`. The mock
core is partial and can SIGSEGV the API mid-request (in-flight call returns
000); the script retries per endpoint, so this is invisible in the demo.
Instance create/reboot is the one gap — blocked by the mock's missing
`FindMachinesByIds`, not by the API (reboot path is code-complete; see
mmoradi-notes/nico/REST-STACK-DEPLOYMENT.md).

## Reset / teardown

```bash
# Reset NICo state (mock fleet re-ingests from scratch):
devspace purge -n nico-system
./dev/deployment/devspace/nuke-postgres.sh
devspace deploy -n nico-system

# Full teardown:
kind delete cluster --name nico
cd demo/frr && docker compose down
```

## Gotchas learned the hard way

- Mock BMC auth is the #1 stall source. NICo's site-explorer logs in with the
   Vault site-wide root credential (root/vault-password). The mocks only accept
   it if mat.toml sets `host_bmc_password`/`dpu_bmc_password = "vault-password"`.
   Without it: Redfish 401s and machines stuck at WAITINGFORPLATFORMPOWERCYCLE.
- After failed logins site-explorer marks endpoints "AvoidLockout" and stops
   probing. Recover with `site-explorer clear-error <ip>` + `refresh <ip>`,
   or a full reset.
- Do NOT `kubectl rollout restart` machine-a-tron alone: mock BMC state lives
   in the pod (`persist_dir=/tmp/...`), so a restart desyncs rotated credentials
   in NICo's DB. Fix = full reset (purge + nuke-postgres + deploy).
- First `devspace deploy` on a fresh docker may race three concurrent builds
   of `build-container-localdev` ("already exists" error). Just re-run; the
   image is present after the first attempt.
- `vpc-peering create` fails with "VPC Peering feature disabled" unless the
   site config sets `vpc_peering_policy = "mixed"` (or "exclusive").
- Switches explore fine but never appear in `switch show` unless
   `[site_explorer] create_switches = true` — its default is false while
   `create_machines` defaults from config. Same family: `create_power_shelves`.
- After `docker compose up --force-recreate` of the whole FRR fleet, FRR can
   come up with L3VNIs recognized but ZERO type-5 origination (startup race).
   Fix: `docker restart <container>` of the affected node(s), then re-run
   vpc-peering-create.sh (runtime `import vrf` state is lost on restart).
- After any DB reset, recreating vpc-blue/vpc-green allocates NEW VNIs (the
   vpc-vni pool is fully auto-assign — explicit `vni` in CreateVpc returns
   ResourceExhausted). The FRR fabric hardcodes the VNIs, so realign:
   `./nico-cli.sh -f json vpc show` to get the new values, then
   `sed -i '' 's/<oldblue>/<newblue>/g; s/<oldgreen>/<newgreen>/g' \
     frr/docker-compose.yml frr/dpu-hbn/frr.conf frr/dpu-hbn/init-evpn.sh \
     frr/dpu-hbn-2/frr.conf frr/tor-leaf/frr.conf`
   and `docker compose down && up -d`. Verify with `show evpn vni` and TWO
   type-5 routes on the tor (2026-07-06: 2024549→2024508, 2024536→2024520).
- Pre-demo checklist: `show bgp l2vpn evpn` on tor must show BOTH tenant
   type-5s; VPCs exist unpeered; interconnect session `Idle (Admin)`.

```text

```