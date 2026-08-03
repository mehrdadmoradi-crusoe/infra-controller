# Model B runbook — NICo on CISv2

Steps to deploy NICo on CISv2 and run the Gate-1 spike. Capture output and
measurements under `results/`.

## 0. Gate 0 first (paper, blocks the rest)
- [ ] Get the two written confirmations from the customer:
      - mediated Redfish-equivalent satisfies req 03
      - provider-executed Flows satisfy the write-access asks
- [ ] Get the hard lines named: Secure-Boot keys, tenant-run DHCP
- [ ] If Gate 0 fails, stop — no point deploying

## 1. Deploy NICo on CISv2
- [ ] `cisv2-deploy` — identify required MRs, Vault secrets, RBAC, ArgoCD app
- [ ] Land manifests under `manifests/`
- [ ] `argocd` — sync the app, confirm health
- [ ] machine-a-tron mock fleet comes up READY; note the count

## 2. Gate 1a — prove the EASY tier is easy
- [ ] Pick one currently provider-only read (e.g. a machine detail field)
- [ ] Expose it to a tenant role: new/opened handler + one RBAC-table line
- [ ] Measure the actual change size + time; record vs the "one line" claim in results/

## 3. Gate 1b — mediated BMC spike (one MEDIUM item)
- [ ] Add tenant principal + per-tenant ACL + BMC mapping to bmc-proxy
      (service-principal-only today; console already proves the tenant-auth path)
- [ ] Tenant power-cycles the host behind their own instance, scoped + audited
- [ ] Verify the DPU BMC is NOT reachable via this path (hard block holds)
- [ ] Record the change + the isolation check

## 4. Gate 1c — provider Flow
- [ ] Kick a Temporal Flow task (e.g. a mock firmware/reclaim workflow)
- [ ] Show status/progress via `GET /task/:id`; confirm it is audited
- [ ] This is the "provider-executed audited workflow" answer to write-access asks

## 5. Write up
- [ ] results/gate1-findings.md — did EASY hold? did mediated BMC work + stay isolated?
- [ ] Feed the measured numbers back into the sizing estimate (Gate 2 input)

## Notes / pitfalls
- CISv2 + machine-a-tron mocks the hardware — this track does NOT prove
  isolation or real BMC behavior (that is Model A).
- Watch build/resource pressure; deploy sequentially if Rust images thrash.
