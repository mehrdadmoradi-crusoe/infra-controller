# Model A — zero-trust bare metal, standalone POC

Prove the **hard-boundary** bare-metal model on **one real BF3 node, by hand,
with no NICo**. The tenant gets a full ring-0 host whose network, storage, and
boot all come from the DPU, and the host provably cannot cross the line to the
DPU.

This is the hardware/datapath fail-fast track. It is deliberately standalone:
no NICo control plane, no HBN/EVPN fabric, no tenant API, no attestation
service. Just enough to demonstrate the trust boundary.

## The four properties (what "zero-trust" means here)

1. Host boots from **DPU-emulated NVMe** (SNAP) — no local boot disk in the path.
2. Host network is delivered **only via the DPU as a NIC** (VF/representor).
3. The operator plane runs **on the DPU ARM**; the host carries no in-band agent.
4. The host **cannot compromise the DPU** — the boundary is structural.

## Success criteria — the proof matrix

Positive (must PASS):
- [ ] Host boots off the DPU-served volume; local disk absent from boot path
- [ ] Host gets network + storage entirely through the DPU
- [ ] Workload runs; same-host datapath shows hardware offload (`dpu-datapath-test`)

Negative / red-team from the host (each must be BLOCKED = PASS):
- [ ] rshim to the DPU ARM — disabled
- [ ] reach the DPU BMC — unreachable (segmented OOB)
- [ ] reflash the DPU / read secure-boot keys — denied

Out-of-band (operator-only, must PASS):
- [ ] Operator reboots / power-cycles / opens serial console with zero host cooperation

## Hardware

- 1 host + its BlueField-3 DPU (dual-port: p0 network, p1 storage)
- Isolated OOB network for the DPU BMC (not reachable from the host)

## Skills this composes

`find-bf3-nodes` -> `dpu-bm` (SNAP-4 boot) -> `dpu-dual-port` -> `dpu-provision`
-> `dpu-datapath-test`, plus `dpu-diag` for mode toggle and the isolation checks.

## What this does NOT prove

Control plane, tenant API, scale — that is Model B. Secure-Boot key delegation
and raw BMC are shared-trust (Model B) items and are explicitly out of scope
here; Model A's job is to show the hard-boundary model works on real silicon.

See `runbook.md` for the step-by-step.
