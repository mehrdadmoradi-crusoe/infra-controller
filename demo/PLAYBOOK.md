# NICo Demo — Run-and-Observe Playbook

Open this file in **VS Code with the Runme extension** ("Runme" by Stateful).
Every command block below gets a ▶ button; output appears inline. Run **top to
bottom** the first time. Paths are relative to this file (`~/infra-controller/demo/`).

Status legend: ✅ verified working · ⚠️ works but finicky · 🧰 needs extra setup.
Last verified 2026-07-24 on colima 4 CPU / 12 GB (peering + interconnect now
survive a DPU flap; see the route-target / rp_filter notes below).

---

## Coverage — what each section proves

| Section | Proves | Status |
|---|---|---|
| §0 Bring-up | one idempotent, self-healing command readies the whole fabric | ✅ |
| §1 Platform | NICo control plane + the 3-node EVPN fabric are up | ✅ |
| §2 The rack is one system | GB200 NVL72 driven as a first-class API object — no console, no BMC | ✅ |
| §3 Zero-touch inventory | whole fleet + DPUs + switches converged unattended (Day-0) | ✅ |
| §4 Tenant API (gRPC) | VPCs created via the API; NICo auto-allocates each a VNI | ✅ |
| §5 VPC peering (money demo) | isolation is structural (VRF per VPC); peer → 0% loss, unpeer → 100% | ✅ |
| §6 EVPN under the hood | real BGP/EVPN type-5 + kernel VRF (table-id = the NICo VNI) | ✅ |
| §7 Cloud interconnect | external cloud in one BGP session → EVPN type-5; no OpenFlow, no VLAN | ✅ |
| §8 Failure & reconvergence | DPU dies → fabric reconverges in ~12 s, no operator | ⚠️ |
| §9 Tenant REST API | live Keycloak JWT + 11 authenticated GETs = 200 (~60-op surface) | ✅ |

Not demonstrable in the hardware-free sim (honest gaps): secure reclaim/sanitize
timing, NVLink partitioning (delegated to NVIDIA's NMX-C/RMS), on-hardware
BMC/PXE/DOCA. A live instance reboot is blocked by NVIDIA's partial mock core.

---

## Presenter track — the story and the lines

**The arc (one breath):** a rack is the unit, not the node → NICo runs the whole
rack as one API object → the network *and* the boot disk live on the DPU
(zero-trust) → the tenant drives all of it through a cloud API → and it's the
same architecture an NVIDIA-partner cloud (Lambda) already ships. Everything
below is the proof.

- **§2 Rack is one system.** Run `rack list` / `rack show`. Say: *"An NVL72 is 72
   GPUs as one system. NICo forms it, enumerates its compute trays and NVLink
   switches, and drives it as a first-class API object — no console, no BMC."*
   Punchline: *"The rack, not the node, is the unit."* Honesty: NVLink
   partitioning delegates to NVIDIA's NMX-C/RMS (the handoff table is empty here).
- **§3 Zero-touch inventory.** Say: *"The whole fleet discovered, attested, and
   converged with zero per-node steps."* Punchline: *"Day-0 is unattended."*
- **§4 Tenant API.** Say: *"Two VPCs, created through the API; NICo auto-allocates
   each a VNI from the site pool."* Punchline: *"Networking is an API object, not a
   ticket."*
- **§5 VPC peering — the money demo.** Run `peer` (0% loss), then `unpeer` (100%).
   Say: *"Isolation is structural — a separate kernel VRF per VPC on every DPU, not
   a firewall rule. Peering is one API object; `forge-dpu-agent` leaks routes
   between those VRFs on every BlueField and the traffic rides the VXLAN fabric as
   EVPN type-5."* Punchline: *"Peer → 0% loss, unpeer → 100%. Isolation is the
   architecture, not a filter."*
- **§6 EVPN under the hood.** Say: *"This is real FRR/BGP — type-5 routes, and a
   kernel VRF per VPC whose routing-table id IS the NICo-allocated VNI. The ToR is
   pure transit; it never holds a tenant prefix."* Punchline: *"Not a mock — real
   EVPN control plane plus kernel state."*
- **§7 Cloud interconnect.** Run `interconnect-up` (ping GCP), then `-down`. Say:
   *"A tenant reaches an external cloud in ONE BGP session inside the customer VRF;
   the prefix returns as EVPN type-5 to every DPU. AS 16550 is the real GCP Partner
   Interconnect ASN."* Punchline (vs the OVN/OpenFlow MVP): *"No BGP-to-OpenFlow
   translation, no per-interconnect VLAN — the interconnect is just one more BGP
   session in a VRF."*
- **§9 Tenant REST API.** Run `tenant-api-tour.sh`. Say: *"This is what the
   customer actually touches — org-scoped REST, a real Keycloak JWT, RBAC. Eleven
   authenticated GETs, all 200. Reboot and console are the same authenticated
   surface (PATCH/POST)."* Honesty: *"A literal reboot needs a machine to cycle —
   blocked by NVIDIA's partial mock core, not by our stack; the reboot path is
   code-complete."*

**Closing line:** *"Everything you saw is open-source NICo on a real EVPN fabric —
the same architecture Lambda ships today. What's left needs silicon, not code."*

## Topology — what you're driving

```text
 kind "nico"  (NICo control plane)        docker net nico-fabric  (172.30.0.0/24 underlay)
 └ ns nico-system                         ├ tor-leaf-01  AS 65100   EVPN transit + border leaf
   ├ nico-api        Forge gRPC :1079     ├ dpu-hbn-01   AS 65201   VTEP 10.180.62.1  vrf-blue  10.10.10.1/24
   ├ nico-bmc-proxy  Redfish              ├ dpu-hbn-02   AS 65202   VTEP 10.180.62.2  vrf-green 10.20.20.1/24
   └ machine-a-tron  mock fleet           └ gcp-router   AS 16550   external cloud · GCP VPC 10.128.0.0/20
     10 hosts · 20 BF3 DPUs · 2 NVOS sw                            dedicated port 172.31.0.0/24
```

Each `dpu-hbn` is the HBN/FRR side of a real BlueField-3. Per DPU: one kernel
VRF per VPC (**table-id = the VPC's NICo-allocated VNI**) plus an L3VNI (bridge
SVI + a VXLAN device sourced from the VTEP loopback). Tenant routes ride **EVPN
type-5 over VXLAN**; the ToR is underlay + EVPN transit and never holds a tenant
prefix. A distinct local AS per DPU (65201/65202) mirrors the real per-BlueField
eBGP model, and **AS 16550 is the actual GCP Partner Interconnect ASN** (the
`10.128.0.0/20` block matches the SSI production deployment). Per-DPU peering /
interconnect route-leaking is exactly what NICo's `forge-dpu-agent` programs via
NVUE on each BlueField.

Run this any time for the **live** logical topology — it reads real fabric state,
so the peering edge and the interconnect link change colour as you run §5 / §7
(green = active, red = isolated/down). Re-run it right after peer / unpeer /
interconnect to show the change on screen:

```sh {"name":"topology"}
./topology.sh
```

## 0. Bring it all up (one command)

`pb-seed.sh` is the self-healing bring-up: it ensures the demo VPCs exist in
NICo, reads the VNIs NICo allocated, aligns the fabric (kernel VRFs + frr.conf
L3VNIs) to them, warm-restarts the fabric, disables NIC offload (VXLAN's
checksum offload doesn't survive a colima reboot — sim-only), and polls until
both DPUs originate their routes. Idempotent — safe to re-run any time,
including after a colima restart or a NICo DB reset.

```sh {"name":"bring-up"}
./pb-seed.sh
```

Expect it to end with `ALIGNED ✓`. Then run `./pb-peer.sh` in §5.

## 1. Platform — what's running

```sh {"name":"colima"}
colima status
```

```sh {"name":"containers"}
docker ps --format 'table {{.Names}}\t{{.Status}}' | grep -E 'hbn|tor-leaf|gcp-router|nico-control'
```

```sh {"name":"nico-pods"}
kubectl get pods -n nico-system | grep -vE 'Completed'
```

## 2. The rack is one system (Demo 2)

The headline: an NVL72 rack is one system, driven as a first-class API object —
no console, no BMC. The declared profile and the rack NICo formed:

```sh {"name":"expected-rack"}
./nico-cli.sh expected-rack show
```

```sh {"name":"rack-list"}
./nico-cli.sh rack list
```

The rack enumerates its own hardware — GB200 compute trays + NVLink (NVOS)
switches — and reports state:

```sh {"name":"rack-show"}
RID=$(./nico-cli.sh rack list 2>/dev/null | grep -v IGNORING \
  | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
./nico-cli.sh rack show "$RID"
```

The GB200 trays converge to READY zero-touch, like any node:

```sh {"name":"gb200-trays"}
./nico-cli.sh machine show 2>/dev/null | grep -v IGNORING | grep -iE 'NVIDIA' | head
```

The one step the sim doesn't run: NVLink fabric bring-up delegates to NVIDIA's
NMX-C/RMS (real service, needs the NVL72 stack). The handoff table it populates
is present but empty here — the honest boundary:

```sh {"name":"nmxc-endpoints"}
./nico-cli.sh nvlink-nmxc-endpoints show
```

## 3. Zero-touch inventory

The whole fleet discovered and converged with no per-node steps:

```sh {"name":"machines"}
./nico-cli.sh machine show | grep -v IGNORING | grep -c READY | xargs echo 'machines READY:' | ./hl.sh -r 'READY: [0-9]+'
```

```sh {"name":"dpu-status"}
./nico-cli.sh dpu status
```

```sh {"name":"switches"}
./nico-cli.sh switch show
```

## 4. Tenant API — create VPCs (gRPC)

`pb-seed` already created `vpc-blue`/`vpc-green`. List them and their
NICo-allocated VNIs:

```sh {"name":"vpc-list"}
./nico-cli.sh -f json vpc show 2>/dev/null | grep -v IGNORING \
  | jq -r '.vpcs[] | "\(.metadata.name)  vni=\(.status.vni)"'
```

## 5. VPC peering — the money demo

Two VPCs, isolated by default — isolation is **structural** (a separate kernel
VRF per VPC on every DPU), not an ACL exception. Peering leaks routes between
those VRFs on every DPU (exactly what `forge-dpu-agent` programs on a real
BlueField); cross-VPC traffic then rides the VXLAN fabric as EVPN symmetric-IRB
type-5, ToR as transit only. **Peer → 0% loss:**

```sh {"name":"peer"}
./pb-peer.sh | ./hl.sh "0% packet loss" "PEERED"
```

Show the peered topology — the blue↔green edge turns green:

```sh {"name":"topology-peered"}
./topology.sh
```

**Unpeer → 100% loss (isolation restored):**

```sh {"name":"unpeer"}
./pb-unpeer.sh | HL_SGR='1;97;41' ./hl.sh "100% packet loss" "ISOLATED"
```

## 6. EVPN fabric — under the hood

Both DPUs peer EVPN with the ToR leaf:

```sh {"name":"bgp-summary"}
docker exec tor-leaf-01 vtysh -c 'show bgp l2vpn evpn summary'
```

Type-5 (IP-prefix) routes each DPU originates for its VPC subnets:

```sh {"name":"type5"}
docker exec dpu-hbn-01 vtysh -c 'show bgp l2vpn evpn' | grep -E '\[5\]|10.10.10|10.20.20' | ./hl.sh -r '\[5\]'
```

Kernel state — a VRF per VPC, table id = the NICo VNI:

```sh {"name":"kernel-vrf"}
docker exec dpu-hbn-01 ip -d link show vrf-blue | grep -oE 'table [0-9]+'
docker exec dpu-hbn-01 ip route show vrf vrf-blue
```

## 7. Cloud interconnect (L3 architecture)

The customer VRF on the border leaf holds one eBGP session to the peer cloud
(`gcp-router`, AS 16550); external prefixes come back as EVPN type-5 to every
DPU. Enabling the interconnect is one BGP session.

Reliable after `pb-seed`: each DPU's vrf-blue now imports the blue L3VNI's
route-target from every fabric ASN (border 65100 + DPUs 65201/65202), so the
border leaf's re-originated GCP type-5 actually installs. Run §5 (peer) first,
then this. If a ping ever fails right after a cold start, the old nudge still
works: `docker exec tor-leaf-01 vtysh -c 'clear bgp vrf vrf-blue *'`, wait ~10s.

**Why this beats the OVNGW/ovnbgp MVP** (say this out loud): route exchange is
plain BGP → EVPN type-5 — no BGP → OpenFlow translation layer; isolation is the
VRF/VNI itself, so there is no per-interconnect VLAN to plumb across the fabric
(only the physical handoff keeps one); multi-VPC per site is just more VRFs; and
prefixes are exact — no supernet restriction, no static steering routes.

```sh {"name":"interconnect-up"}
frr/interconnect-up.sh
sleep 8
docker exec dpu-hbn-01 ip vrf exec vrf-blue ping -c 3 -W 1 -I 10.10.10.1 10.128.0.1 | ./hl.sh "0% packet loss"
./topology.sh
```

Withdraw it → the GCP prefix disappears, VPC unreachable:

```sh {"name":"interconnect-down"}
frr/interconnect-down.sh
sleep 5
docker exec dpu-hbn-01 ip vrf exec vrf-blue ping -c 2 -W 1 -I 10.10.10.1 10.128.0.1 || echo '--- withdrawn: unreachable (expected) ---'
frr/interconnect-up.sh
```

## 8. Failure and reconvergence

⚠️ Re-verify after the fixes this session. Kill a DPU; the fabric detects and
reconverges with zero operator action:

```sh {"name":"kill-dpu"}
docker stop dpu-hbn-01
sleep 3
docker exec tor-leaf-01 vtysh -c 'show bgp l2vpn evpn summary'
```

Revive it — it re-inits (offload off, VNIs re-sourced) and BGP reconverges:

```sh {"name":"revive-dpu"}
docker start dpu-hbn-01
echo 'waiting ~15s for reconverge...'
sleep 15
docker exec tor-leaf-01 vtysh -c 'show bgp l2vpn evpn summary'
```

If peering was active, re-apply it (runtime import state is lost on restart):

```sh {"name":"re-peer"}
frr/vpc-peering-create.sh
```

## 9. Tenant REST API + Keycloak (JWT) — the customer-facing API

This is the _tenant_ surface (§4 was the operator/Forge gRPC): org-scoped REST
`/v2/org/{org}/nico/{resource}`, real Keycloak JWT, RBAC (TENANT_ADMIN) — the
same authenticated API a customer drives, no BMC, no hypervisor. It runs on a
__separate__ kind cluster `nico-rest-local` (Keycloak :8082, API :8388): the
full stack — API, cloud/site workers, site-manager, mock-core, Keycloak,
Temporal (mTLS), Postgres, cert-manager.

The catalog prints the whole ~60-op surface and needs no stack:

```sh {"name":"tenant-catalog"}
./tenant-api-tour.sh --catalog
```

The live proof mints a real tenant JWT and runs 11 authenticated GETs (all 200):

```sh {"name":"tenant-live"}
./tenant-api-tour.sh
```

Live-proof note: `200`/empty-list means the endpoint was served, the JWT was
accepted, and RBAC passed — reboot/console/delete are the same authenticated
surface (PATCH/POST). The one gap is a machine actually *cycling*: NVIDIA's mock
core is partial (`FindMachinesByIds` unimplemented), so instance create/reboot
can't complete — a limit of the test double, not the API (reboot path is
code-complete). Honest boundary to state out loud.

If the stack is down (fresh laptop / cluster deleted), bring it back with the
maintained deploy (reuses cached images, ~15 min; see
`mmoradi-notes/nico/REST-STACK-DEPLOYMENT.md`):

```sh {"interactive":"true","name":"tenant-stack-up"}
cd ~/infra-controller/rest-api && make -o docker-build-local kind-reset-kustomize
# then in the nico-rest-local cluster, one-time: set the demo user's password
KC=$(kubectl -n nico-rest get pod -l app=keycloak -o name | head -1)
kubectl -n nico-rest exec "$KC" -- /opt/keycloak/bin/kcadm.sh config credentials \
  --server http://localhost:8080 --realm master --user admin --password admin
kubectl -n nico-rest exec "$KC" -- /opt/keycloak/bin/kcadm.sh set-password \
  -r nico-dev --username testuser --new-password demo
```

Gotchas if the deploy is ever rebuilt from scratch: the VM needs
`fs.inotify.max_user_instances` raised (default 128 fails cluster create with
other kind clusters running — `colima ssh -- sudo sysctl -w
fs.inotify.max_user_instances=8192`); and the `site-manager` cert still carries
the upstream `carbide-rest-*` names, so clone the CA issuer
(`carbide-rest-ca-issuer` → same `ca-signing-secret`) and add a
`nico-rest-site-manager` Service alias. The 11 tenant GETs work without the
site-manager being fully ready.

---

## Reset (destructive — fleet re-ingests for ~5 min)

Only for a clean slate. After a DB reset, VNIs change — just re-run
`./pb-seed.sh` (§0), which re-aligns everything.

```sh {"name":"reset"}
cd .. && devspace purge -n nico-system && ./dev/deployment/devspace/nuke-postgres.sh && devspace deploy -n nico-system
```
