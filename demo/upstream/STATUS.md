# Upstream contributions tracker

Account: mehrdadmoradi-crusoe (verified via `gh api user` 2026-07-06),
affiliation "DPU/bare-metal provisioning at Crusoe (NVIDIA Cloud Partner)"
stated in issue bodies.

## Filed

| # | Repo | Issue | Status |
|---|------|-------|--------|
| 1 | NVIDIA/infra-controller | [#3105](https://github.com/NVIDIA/infra-controller/issues/3105) kind-load hook fails for clusters not named "kind" | filed 2026-07-02, PR offered |
| 2 | NVIDIA/infra-controller | [#3106](https://github.com/NVIDIA/infra-controller/issues/3106) first-deploy race on build-container-localdev | filed 2026-07-02, PR offered |
| 3 | NVIDIA/infra-controller | [#3152](https://github.com/NVIDIA/infra-controller/issues/3152) create_switches silently off — docs/logging request | filed 2026-07-06 |
| 4 | NVIDIA/infra-controller | [#3153](https://github.com/NVIDIA/infra-controller/issues/3153) vpc-peering delete docs show positional ID, CLI wants --id | filed 2026-07-06, docs PR offered |
| 5 | NVIDIA/infra-controller | [#3158](https://github.com/NVIDIA/infra-controller/issues/3158) local-dev mock BMC creds never align out of the box | filed 2026-07-06, isolation caveat stated |
| 6 | NVIDIA/infra-controller | [#3159](https://github.com/NVIDIA/infra-controller/issues/3159) GB300/VR tray mocks fail MachineId generation + ERROR spam | filed 2026-07-06 |
| 7 | FRRouting/frr | [#22577](https://github.com/FRRouting/frr/issues/22577) per-VRF SVI MACs on one VTEP silently blackhole | filed 2026-07-06 |

## Pending (in priority order)

3. ~~Local dev broken out of the box~~ — FILED as #3158 (with the
   isolation caveat stated in the body; test still worth running post-meeting).
4. **PR for #3105** — one-line fix, ready in working tree. Needs SSH signing
   key on the GitHub account (branch protection requires signed commits +
   DCO sign-off: `git commit -s -S`).
5. **PR for #3106** — before:build hook approach, wait for maintainer nod on
   the issue first.
6. ~~create_switches silently off~~ — FILED as #3152.
7. ~~MachineId-generation ERROR spam~~ — FILED as #3159; root-caused to
   GB300/VR mock serial fields (2026-07-06: 120/10min from exactly the 2
   affected endpoints). Likely also explains the 3 missing GB300 demo trays.
8. ~~FRRouting/frr silent blackhole~~ — FILED as FRRouting/frr#22577.
9. ~~Docs correction (vpc-peering delete --id)~~ — FILED as #3153.

## Contribution requirements (NVIDIA/infra-controller)

- DCO sign-off AND crypto signature on every commit: `git commit -s -S`
- Fork workflow, branch names like `fix/...`
- One focused PR per change; PR template; evidence-backed claims
- Project is experimental; issue-first for anything non-trivial