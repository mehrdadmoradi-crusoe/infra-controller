# Model A runbook — standalone zero-trust POC

Repeatable steps to stand up and prove the hard-boundary model on one BF3 node.
Fill in node-specific values (hostname, BMC IPs, image) as you go; capture
command output under `results/`.

## 0. Pick a node
- [ ] `find-bf3-nodes` — shortlist a free BF3/B200 box, note its lab-mgr reservation
- [ ] Record: host, DPU BMC IP, host BMC IP, OOB segment

## 1. DPU mode + dual-PF
- [ ] Put the DPU in DPU/zero-trust (embedded) mode (`dpu-diag` mode toggle)
- [ ] Dual-PF: p0 for host network via OVS-DOCA, p1 for storage (`dpu-dual-port`)
- [ ] Confirm host sees the DPU as a NIC only (VF/representor present)

## 2. Boot from DPU (SNAP)
- [ ] Enable SNAP NVMe emulation, attach a boot volume served by the DPU (`dpu-bm`)
- [ ] Remove/ignore the host's local disk so it is not in the boot path
- [ ] Boot the host off the emulated volume; capture the boot log to `results/`

## 3. Network datapath
- [ ] Plain OVS-DOCA bridge on the ARM (no HBN/EVPN needed standalone)
- [ ] Host NIC comes up, gets its address, reaches the network via the DPU
- [ ] `dpu-datapath-test` — same-host iperf proves hardware offload; save numbers

## 4. Lock the boundary
- [ ] Disable rshim from the host side
- [ ] Verify the DPU BMC sits on an isolated OOB segment the host cannot route to
- [ ] Confirm DPU secure boot is on and NIC firmware is locked

## 5. Prove it (the point of the POC)
Run each and record PASS/FAIL in `results/proof-matrix.md`:

Positive:
- [ ] host booted from DPU volume, no local disk in path
- [ ] network + storage delivered by the DPU
- [ ] workload runs; offload confirmed

Negative (from the host — must be blocked):
- [ ] `rshim` to DPU ARM -> blocked
- [ ] ping/ssh the DPU BMC -> unreachable
- [ ] attempt DPU reflash / key read -> denied

Out-of-band (operator side — must work without host):
- [ ] reboot / power-cycle via BMC
- [ ] serial console via BMC

## 6. Write up
- [ ] Fill `results/proof-matrix.md` with the outcomes + evidence
- [ ] Note anything that did NOT hold as expected (those are the real findings)
