# Bare-metal POCs — two independent tracks

We are validating two different bare-metal trust models. The POCs are kept
**independent on purpose**: different hardware, different success criteria,
different failure modes. Progress on one should never block or entangle the
other.

## The two models

| | Model A — zero-trust NIC | Model B — shared-trust / NICo |
|---|---|---|
| Boundary | Hard line at the DPU (structural, one-way) | Boundary pushed up; tenant reaches platform via a broker |
| DPU | Presents as a NIC (+ SNAP NVMe); operator-owned | Same DPU, plus a scoped, audited mediation layer |
| What we prove | The trust boundary + datapath on real silicon | The control plane, tenant API, and mediated BMC |
| Track | Hardware / datapath (this folder, `model-a-...`) | Control plane on CISv2 (this folder, `model-b-...`) |
| Fail-fast question | "Does the hard-boundary model actually hold on a BF3?" | "Can NICo deliver the customer asks without sharing trust?" |

Slides that frame this:
- Zero-trust vs shared-trust: https://claude.ai/code/artifact/adc46c91-0c91-44aa-8f97-2b3df6493093
- NICo's answer (keep the boundary, broker the access): https://claude.ai/code/artifact/e8b5f078-d0ed-4b04-9ff1-70cd441cefff

## Why independent

- **Model A** needs a real BF3/B200 node and proves things a mock can't
  (isolation, SNAP boot, offload). No NICo required.
- **Model B** needs a persistent k8s environment (CISv2) and proves the
  software/control-plane story. Hardware is mocked (machine-a-tron).

Conflating them hides which risk is actually being retired. Keep the runbooks,
configs, and results separate; only the conclusions meet again (in the deck).

## Status

| POC | Owner | State |
|---|---|---|
| model-a-zerotrust-standalone | mmoradi | not started — runbook drafted |
| model-b-nico-cisv2 | mmoradi | not started — runbook drafted |
