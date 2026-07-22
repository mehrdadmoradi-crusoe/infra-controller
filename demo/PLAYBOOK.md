# NICo Demo — Run-and-Observe Playbook

Open this file in **VS Code with the Runme extension** ("Runme" by Stateful).
Every command block below gets a ▶ button; output appears inline. Run **top to
bottom** the first time. Paths are relative to this file (`~/infra-controller/demo/`).

Status legend: ✅ verified working · ⚠️ works but finicky · 🧰 needs extra setup.
Last verified 2026-07-22 on colima 4 CPU / 12 GB.

---

## Coverage — what each section proves

| Section | Proves | Status |
|---|---|---|
| §0 Bring-up | one command readies the whole fabric | ✅ |
| §1 Platform | control plane + fabric are up | ✅ |
| §2 The rack is one system | GB200 rack as a first-class API object (Demo 2) | ✅ |
| §3 Zero-touch inventory | fleet + DPUs + switches converged unattended | ✅ |
| §4 Tenant API | VPCs created via the API | ✅ |
| §5 VPC peering (money demo) | peer → 0% loss, unpeer → 100% | ✅ |
| §6 EVPN under the hood | real BGP/EVPN control + kernel state | ✅ |
| §7 Cloud interconnect | BGP to an external cloud, routes as EVPN type-5 | ⚠️ |
| §8 Failure & reconvergence | DPU dies → fabric reconverges, no operator | ⚠️ |
| §9 Tenant REST API | the ~60-op Keycloak-authed customer API | 🧰 |

Not demonstrable in the hardware-free sim (honest gaps): secure reclaim/sanitize
timing, NVLink partitioning (delegated to NVIDIA's NMX-C/RMS), on-hardware
BMC/PXE/DOCA. A live instance reboot is blocked by NVIDIA's partial mock core.

---

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
./nico-cli.sh machine show | grep -v IGNORING | grep -c READY | xargs echo 'machines READY:'
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

Two VPCs, isolated by default. Peering leaks routes between the per-VPC VRFs on
every DPU (exactly what `forge-dpu-agent` does on a real BlueField), and traffic
flows over the VXLAN fabric. **Peer → 0% loss:**

```sh {"name":"peer"}
./pb-peer.sh
```

**Unpeer → 100% loss (isolation restored):**

```sh {"name":"unpeer"}
./pb-unpeer.sh
```

## 6. EVPN fabric — under the hood

Both DPUs peer EVPN with the ToR leaf:

```sh {"name":"bgp-summary"}
docker exec tor-leaf-01 vtysh -c 'show bgp l2vpn evpn summary'
```

Type-5 (IP-prefix) routes each DPU originates for its VPC subnets:

```sh {"name":"type5"}
docker exec dpu-hbn-01 vtysh -c 'show bgp l2vpn evpn' | grep -E '\[5\]|10.10.10|10.20.20'
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

⚠️ **Finicky:** after a fresh bring-up the border leaf sometimes doesn't
re-originate the GCP prefix as type-5. If the ping below fails, nudge it with
`docker exec tor-leaf-01 vtysh -c 'clear bgp vrf vrf-blue *'`, wait ~10s, retry.

```sh {"name":"interconnect-up"}
frr/interconnect-up.sh
sleep 8
docker exec dpu-hbn-01 ip vrf exec vrf-blue ping -c 3 -W 1 -I 10.10.10.1 10.128.0.1
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

## 9. Tenant REST API + Keycloak (JWT) — needs the REST stack

🧰 The tenant-facing REST API (org-scoped `/v2/org/{org}/nico/{resource}`,
Keycloak JWT, TENANT_ADMIN) runs on a separate kind cluster `nico-rest-local`
(Keycloak :8082, API :8388) — **not up by default**. Catalog needs no stack:

```sh {"name":"tenant-catalog"}
./tenant-api-tour.sh --catalog
```

With the stack up (see `mmoradi-notes/nico/REST-STACK-DEPLOYMENT.md`), the live
proof mints a real tenant JWT and runs ~11 authenticated GETs (all 200):

```sh {"name":"tenant-live"}
./tenant-api-tour.sh
```

---

## Reset (destructive — fleet re-ingests for ~5 min)

Only for a clean slate. After a DB reset, VNIs change — just re-run
`./pb-seed.sh` (§0), which re-aligns everything.

```sh {"name":"reset"}
cd .. && devspace purge -n nico-system && ./dev/deployment/devspace/nuke-postgres.sh && devspace deploy -n nico-system
```
