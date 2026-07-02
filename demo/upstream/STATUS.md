# Upstream contributions tracker

Account: imehrdad2012 (personal), affiliation "DPU/bare-metal provisioning at
Crusoe (NVIDIA Cloud Partner)" stated in issue bodies.
Repo-local git identity: Mehrdad Moradi <imehrdad2012@gmail.com>.

## Filed

| # | Repo | Issue | Status |
|---|------|-------|--------|
| 1 | NVIDIA/infra-controller | [#3105](https://github.com/NVIDIA/infra-controller/issues/3105) kind-load hook fails for clusters not named "kind" | filed 2026-07-02, PR offered |
| 2 | NVIDIA/infra-controller | [#3106](https://github.com/NVIDIA/infra-controller/issues/3106) first-deploy race on build-container-localdev | filed 2026-07-02, PR offered |

## Pending (in priority order)

3. **Local dev broken out of the box** (mat.toml missing `vault-password`
   alignment with Vault site-root cred) — strongest finding, BLOCKED on an
   isolation test: fresh DB with ONLY the mat.toml fix (no factory-default
   credential seeding) must reach Ready to prove the minimal fix.
4. **PR for #3105** — one-line fix, ready in working tree. Needs SSH signing
   key on the GitHub account (branch protection requires signed commits +
   DCO sign-off: `git commit -s -S`).
5. **PR for #3106** — before:build hook approach, wait for maintainer nod on
   the issue first.
6. **create_switches silently off** — file as docs/logging request, not bug
   (may be a deliberate safe default).
7. **MachineId-generation ERROR spam** — NOT switch-specific as first
   thought (fires for host/DPU endpoints too, 36/5min on a healthy site);
   needs root-cause before filing. AvoidLockout-mitigation-text claim was
   confounded — dropped unless re-verified.
8. **FRRouting/frr** — silent blackhole when one VTEP advertises distinct
   per-VRF router-MACs (zebra last-write-wins on SVI neigh entries, no
   warning). Repro = demo/frr compose file with the shared-MAC fix reverted.
9. **Docs correction**: `docs/manuals/vpc/vpc_peering_management.md` shows
   `vpc-peering delete <PEERING_CONNECTION_ID>` (positional) but the CLI
   requires `--id <ID>` — the documented command exits with a usage error.
   Easy docs PR or use their documentation_request_correction issue form.

## Contribution requirements (NVIDIA/infra-controller)

- DCO sign-off AND crypto signature on every commit: `git commit -s -S`
- Fork workflow, branch names like `fix/...`
- One focused PR per change; PR template; evidence-backed claims
- Project is experimental; issue-first for anything non-trivial