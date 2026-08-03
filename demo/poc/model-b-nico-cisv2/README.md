# Model B — NICo control plane on CISv2, POC

Prove the **shared-trust asks can be satisfied without collapsing zero-trust** —
i.e. NICo keeps the DPU boundary hard and delivers the customer capabilities
through a scoped, audited broker. This is the software/control-plane fail-fast
track, run on a persistent CISv2 environment (hardware mocked via
machine-a-tron).

Frame: keep the hard boundary, broker the access.
- NICo's answer slide: https://claude.ai/code/artifact/e8b5f078-d0ed-4b04-9ff1-70cd441cefff

## What this proves

- NICo deploys and runs on CISv2 (persistent, GitOps, real Vault/Keycloak/RBAC)
- The **EASY tier is actually easy**: expose an already-ingested capability to a
  tenant role and measure the "one RBAC line + a handler" claim
- **Mediated BMC** works: a tenant power-cycles the host behind their own
  instance, scoped and audited — DPU BMC never reachable
- A **provider Flow** (Temporal) delivers an audited, status-trackable workflow

## Success criteria — the gates

- [ ] **Gate 0 (paper):** customer confirms mediated Redfish satisfies req 03 and
      provider-executed Flows satisfy the write-access asks; names the
      Secure-Boot-key and DHCP hard lines
- [ ] **Gate 1 (spike, ~2 wks):** NICo up on CISv2; prove the EASY tier live +
      one MEDIUM projection (tenant-scoped BMC read/power)
- [ ] **Gate 2 (slice, ~4-6 wks):** one requirement per bin end-to-end; replace
      the order-of-magnitude estimate with a bottoms-up number

## The model changes under test (from the assessment)

- Easy (expose on Instance): power verbs, scoped `bmcAccess`, `bootConfig`, firmware read
- Medium (scope the broker): tenant principals + per-tenant ACL + BMC mapping on bmc-proxy
- Medium (new resources): delegated prefixes, custody events, Flow-as-ticket
- Trust decision (negotiate, do NOT build here): tenant Secure-Boot keys, tenant-run DHCP

## Environment

- CISv2 cluster (persistent) — deploy via `cisv2-deploy`, sync via `argocd`
- machine-a-tron mock fleet (hardware is mocked; this track is about the control plane)
- Vault / Keycloak / RBAC wired for real

## What this does NOT prove

Hardware truth — isolation, SNAP boot, real BMC/DPU behavior. That is Model A.
Keep the two separate; only the conclusions meet in the deck.

See `runbook.md` for the deploy + spike steps.
