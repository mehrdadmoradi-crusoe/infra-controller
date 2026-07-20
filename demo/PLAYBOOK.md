# NICo Demo — Interactive Playbook

Open this file in VS Code with the **Runme** extension installed (search
"Runme" by Stateful in Extensions). Every block below gets a ▶ button;
output appears inline. Blocks are ordered — run top to bottom the first time.

All commands verified working on this machine (§1–§7 on 2026-07-02; §8 rack
lifecycle re-verified 2026-07-20). Paths are relative to this file's directory
(`~/infra-controller/demo/`).

---

## Coverage — what this proves, mapped to the deck

| Deck claim ("What we've validated") | Section | Live now |
|---|---|---|
| Zero-touch fleet bringup | §2 Inventory | yes |
| Tenant provisioning via API (CreateVpc) | §3 Tenant gRPC API | yes |
| Isolation & peering | §3 (pb-peer/unpeer), §6 | yes |
| Cloud interconnect | §6b | yes |
| Failure recovery | §7 | yes |
| Supercluster rack lifecycle | §8 Rack lifecycle | yes |
| Node / instance lifecycle | §9 (API surface; seed to demo) | partial |
| Tenant control plane — REST + Keycloak/JWT | §10 tenant-api-tour.sh | needs REST stack up |

Deliberately **not** in this playbook — not demonstrable without real hardware (the
deck's "What is not yet proven" slide says so): secure reclaim/sanitize timing on
real disks + firmware, NVLink partitioning (delegated to NVIDIA's NMX-C/RMS), and
on-hardware BMC/Redfish/PXE/DOCA.

---

## 1. Platform — what's running

```sh {"name":"colima-status"}
colima status
```

```sh {"name":"docker-containers"}
docker ps --format 'table {{.Names}}\t{{.Status}}'
```

```sh {"name":"nico-pods"}
kubectl get pods -n nico-system
```

```sh {"name":"bootstrap-pods"}
kubectl get pods -n postgres
kubectl get pods -n vault
kubectl get pods -n cert-manager
```

## 2. NICo inventory (admin CLI)

The `IGNORING SERVER CERT` warnings are expected in this dev setup.

```sh {"name":"machines"}
./nico-cli.sh machine show
```

```sh {"name":"dpu-status"}
./nico-cli.sh dpu status
```

```sh {"name":"switches"}
./nico-cli.sh switch show
```

```sh {"name":"expected-switches"}
./nico-cli.sh expected-switch show
```

```sh {"name":"managed-hosts-count"}
./nico-cli.sh site-explorer get-report all 2>/dev/null | grep -v IGNORING | jq '.managed_hosts | length'
```

```sh {"name":"mock-fleet-logs"}
kubectl logs -n nico-system deploy/machine-a-tron --tail=15
```

## 3. Tenant gRPC API

Run the setup block first — the port-forward stays up in the background.
Re-run it any time you see `Failed to dial ... EOF`.

```sh {"name":"port-forward"}
pkill -f "port-forward -n nico-system" 2>/dev/null
sleep 1
nohup kubectl port-forward -n nico-system deploy/nico-api 1079:1079 >/tmp/nico-pf.log 2>&1 &
disown
sleep 2
grep -m1 "Forwarding from" /tmp/nico-pf.log && echo "port-forward UP (survives this cell)" || cat /tmp/nico-pf.log
```

```sh {"name":"extract-certs"}
mkdir -p /tmp/nico-certs
kubectl exec -n nico-system deploy/nico-api -- \
  tar cf - -C /var/run/secrets/spiffe.io/..data . | tar xf - -C /tmp/nico-certs
ls -l /tmp/nico-certs
```

```sh {"name":"grpc-list-services"}
grpcurl -insecure --cert /tmp/nico-certs/tls.crt --key /tmp/nico-certs/tls.key \
  localhost:1079 list
```

```sh {"name":"grpc-list-forge-methods"}
grpcurl -insecure --cert /tmp/nico-certs/tls.crt --key /tmp/nico-certs/tls.key \
  localhost:1079 list forge.Forge | head -40
```

```sh {"name":"grpc-describe-createvpc"}
grpcurl -insecure --cert /tmp/nico-certs/tls.crt --key /tmp/nico-certs/tls.key \
  localhost:1079 describe forge.VpcCreationRequest
```

```sh {"name":"vpc-list"}
./nico-cli.sh vpc show
```

```sh {"name":"vpc-create"}
grpcurl -insecure --cert /tmp/nico-certs/tls.crt --key /tmp/nico-certs/tls.key \
  -d '{"metadata":{"name":"vpc-playbook"},"tenantOrganizationId":"demo-org"}' \
  localhost:1079 forge.Forge.CreateVpc
```

Cleanup — removes the playbook VPC (and any peerings involving it):

```sh {"name":"vpc-delete"}
./pb-cleanup.sh
```

### VPC peering, end to end (the full story)

One command per direction. `pb-peer.sh` creates the VpcPeering object in
NICo, applies the VRF route leaking on both DPUs (the job forge-dpu-agent
does on real BlueFields), then proves it: leaked route + cross-VPC ping over
VXLAN. `pb-unpeer.sh` reverses all of it and proves isolation is back.

```sh {"name":"vpc-peer-e2e"}
./pb-peer.sh
```

```sh {"name":"vpc-unpeer-e2e"}
./pb-unpeer.sh
```

## 4. EVPN fabric — control plane

```sh {"name":"tor-bgp-summary"}
docker exec tor-leaf-01 vtysh -c 'show bgp summary'
```

```sh {"name":"evpn-type5-routes"}
docker exec dpu-hbn-02 vtysh -c 'show bgp l2vpn evpn'
```

```sh {"name":"evpn-vni-map"}
docker exec dpu-hbn-02 vtysh -c 'show evpn vni'
```

```sh {"name":"evpn-rmacs"}
docker exec dpu-hbn-02 vtysh -c 'show evpn rmac vni all'
```

## 5. EVPN fabric — kernel state

```sh {"name":"dpu1-devices"}
docker exec dpu-hbn-01 ip -br link show
```

```sh {"name":"dpu1-vrf-blue-routes"}
docker exec dpu-hbn-01 ip route show vrf vrf-blue
```

```sh {"name":"dpu2-bridge-fdb"}
docker exec dpu-hbn-02 bridge fdb show dev vni2024549
```

```sh {"name":"dpu2-rmac-neigh"}
docker exec dpu-hbn-02 ip neigh show dev br-vrf-blue
```

## 6. VPC interconnect — the money demo

Run these five in order and watch isolation → peering → live traffic.

```sh {"name":"peering-off"}
frr/vpc-peering-delete.sh
```

```sh {"name":"ping-isolated"}
docker exec dpu-hbn-02 ip vrf exec vrf-green ping -c 2 -W 1 -I 10.20.20.1 10.10.10.1 || echo "--- isolated: 100% loss (expected) ---"
```

```sh {"name":"peering-on"}
frr/vpc-peering-create.sh
sleep 3
docker exec dpu-hbn-02 vtysh -c 'show ip route vrf vrf-green'
```

```sh {"name":"ping-peered"}
docker exec dpu-hbn-02 ip vrf exec vrf-green ping -c 3 -W 1 -I 10.20.20.1 10.10.10.1
```

```sh {"name":"vxlan-counters"}
docker exec dpu-hbn-02 ip -s link show vni2024549
```

## 6b. Cloud VPC Interconnect (L3-architecture version)

The border leaf holds the customer VRF; `gcp-router` (AS 16550, the real GCP
Partner Interconnect ASN) advertises the "GCP VPC" 10.128.0.0/20 over the
dedicated-port network. Enabling the interconnect = one BGP session in the VRF.

```sh {"name":"interconnect-up"}
frr/interconnect-up.sh
```

```sh {"name":"interconnect-verify"}
docker exec tor-leaf-01 vtysh -c 'show bgp vrf vrf-blue summary'
docker exec dpu-hbn-01 vtysh -c 'show ip route vrf vrf-blue'
```

```sh {"name":"ping-vpc-to-gcp"}
docker exec dpu-hbn-01 ip vrf exec vrf-blue ping -c 3 -W 1 -I 10.10.10.1 10.128.0.1
```

```sh {"name":"ping-gcp-to-vpc"}
docker exec gcp-router ping -c 2 -W 1 -I 10.128.0.1 10.10.10.1
```

```sh {"name":"interconnect-down"}
frr/interconnect-down.sh
sleep 5
docker exec dpu-hbn-01 ip vrf exec vrf-blue ping -c 2 -W 1 -I 10.10.10.1 10.128.0.1 || echo "--- withdrawn: unreachable (expected) ---"
frr/interconnect-up.sh
```

## 7. Failure and reconvergence

```sh {"name":"kill-dpu1"}
docker stop dpu-hbn-01
sleep 3
docker exec tor-leaf-01 vtysh -c 'show bgp summary'
```

```sh {"name":"revive-dpu1"}
docker start dpu-hbn-01
echo "waiting 12s for BGP to reconverge..."
sleep 12
docker exec tor-leaf-01 vtysh -c 'show bgp summary'
```

## 8. Supercluster rack lifecycle (Demo 2)

The GB200 NVL72 rack is a first-class API object — the same control plane, one
level up from the node. Verified live 2026-07-20.

The declared rack profile (what the site expects) and the rack object NICo formed:

```sh {"name":"expected-rack"}
./nico-cli.sh expected-rack show
```

```sh {"name":"rack-list"}
./nico-cli.sh rack list
```

The rack enumerates its own hardware — 2 GB200 compute trays + 2 NVLink (NVOS)
switches — and reports state (Discovering). No console, no BMC:

```sh {"name":"rack-show"}
RID=$(./nico-cli.sh rack list 2>/dev/null | grep -v IGNORING \
  | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
./nico-cli.sh rack show "$RID"
```

The GB200 trays converge to READY zero-touch, same as any node:

```sh {"name":"gb200-trays-ready"}
./nico-cli.sh machine show 2>/dev/null | grep -v IGNORING | grep -iE 'NVIDIA' | head
```

The one step this sim does **not** run: NVLink fabric bring-up delegates to
NVIDIA's NMX-C/RMS (a real service, needs the NVL72 stack). The mapping table it
would populate is present but empty here — the honest handoff boundary:

```sh {"name":"nmxc-endpoints"}
./nico-cli.sh nvlink-nmxc-endpoints show
```

## 9. Node / instance lifecycle (API surface)

The tenant node loop — allocate → operate → reclaim — is exposed by the
`instance` / `instance-type` verbs. Nothing is allocated in this sim (empty
tables), so these show the surface; seed an instance-type + instance to demo the
full loop live.

```sh {"name":"instance-verbs"}
./nico-cli.sh instance --help 2>/dev/null | grep -v IGNORING | sed -n '/Commands:/,/^Options:/p'
```

```sh {"name":"instance-types"}
./nico-cli.sh instance-type show
```

```sh {"name":"instances"}
./nico-cli.sh instance show
```

Power/boot as API verbs (the mediated-BMC claim) live under `boot-override` and
`bmc-machine` — operator-mediated, never raw host BMC:

```sh {"name":"boot-verbs"}
./nico-cli.sh boot-override --help 2>/dev/null | grep -v IGNORING | sed -n '/Commands:/,/^Options:/p' | head
```

## 10. Tenant REST API + Keycloak (JWT) — requires the REST stack

The tenant-facing REST API (org-scoped `/v2/org/{org}/nico/{resource}`, Keycloak
JWT, TENANT_ADMIN) runs on a separate kind cluster `nico-rest-local`
(Keycloak :8082, API :8388). It is **not** running by default — bring it up first
(see `REST-STACK-DEPLOYMENT.md`). Then:

```sh {"name":"tenant-api-tour"}
./tenant-api-tour.sh            # ~60-op catalog + a live authenticated probe
```

The catalog alone needs no stack:

```sh {"name":"tenant-api-catalog-only"}
./tenant-api-tour.sh --catalog
```

---

## Reset (destructive — fleet re-ingests for ~5 min)

Only when you want a clean slate. See RUNBOOK.md "One-time seeding" for the
credential commands that MUST follow the redeploy.

```sh {"name":"nico-reset"}
cd .. && devspace purge -n nico-system && ./dev/deployment/devspace/nuke-postgres.sh && devspace deploy -n nico-system
```