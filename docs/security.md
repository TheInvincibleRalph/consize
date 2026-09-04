# Consize — Security Design

Consize is a tool that *mutates production infrastructure*. Its threat model is therefore its own worst case: **compromise of Consize = attacker with rightsizing power over your cluster and databases.** Every design decision below exists to contain that.

## 1. Threat model

| Threat | Impact | Mitigation |
|---|---|---|
| Consize web app compromised | Attacker applies arbitrary changes to workloads/DBs | Least privilege, approvals, audit, network isolation, OIDC |
| Malicious/untrusted UI user | Unauthorized applies | Server-side RBAC (never UI-only), approval flows |
| Leaked service account creds (k8s/cloud) | Same as Consize itself | Scope to minimum, rotate, monitor usage |
| Supply chain: malicious dependency/image | Code execution in Consize runtime | Signed + scanned images, pinned deps, SBOM |
| Audit trail tampering | Undetectable malicious apply | Append-only store (DB grants), log shipping |
| DoS of Consize | Stops analysis; apply blocked (safe-fail) | Rate limits, readiness gating |

**Principle: Consize fails safe.** Any degraded state (store down, k8s unreachable, verification inconclusive) blocks applies — it is always acceptable to *not* right-size; it is never acceptable to apply unverified.

## 2. Identity & access

- **Humans (ADR-037):** local users with bcrypt passwords today, on a
  provider seam (`internal/auth.Authenticator`) that OIDC (Google/GitHub/your
  IdP) implements later — the roles and enforcement below are provider-neutral.
  Roles (stored `viewer|operator|admin`, mapped to `consize:view` /
  `consize:operator` / `consize:admin`):
  - `consize:view` — read-only (a viewer's apply call is rejected server-side, 403)
  - `consize:operator` — approve + apply (dry-run always available)
  - `consize:admin` — policy/settings changes
  Sessions are revocable Postgres rows (7-day TTL, hashed tokens), the
  cookie is httpOnly + SameSite=Lax (+ Secure behind TLS), and the apply
  `actor` in the audit trail is the server-verified session email —
  clients cannot self-report identity. Bootstrap: `CONSIZE_BOOTSTRAP_ADMIN`
  creates the first admin while the users table is empty — or, for ad-hoc
  deploys, the first-run wizard (`POST /auth/setup`, 8-char minimum) creates
  it interactively: same one-admin-ever gate, no default credential, no open
  registration (ADR-037 §6 amendment).
- **Consize → k8s (read):** `consize-reader` ServiceAccount with read-only RBAC. For cluster-wide installations, leave `CONSIZE_NAMESPACES` empty and bind it to the read-only ClusterRole. It discovers workloads and pods; it cannot mutate anything.
- **Consize → k8s (write):** *separate* `consize-writer` ServiceAccount, used only by the apply/rollback engine, with RBAC limited to:
  - `get/list/watch/update deployments` on resources **in explicitly write-enabled namespaces only** (RoleBinding per namespace, never cluster-wide)
  - `mode=auto` additionally requires `consize.savings.dev/auto-apply=enabled`; approved Direct apply requires an authenticated operator and the RoleBinding
  - No permission to create/delete workloads, secrets, configmaps, namespaces, nodes, CRDs, or RBAC objects
- **Consize → cloud DB (read):** IAM policy with only `rds:Describe*`-style reads.
- **Consize → cloud DB (write):** IAM policy with `rds:ModifyDBInstance` restricted **by resource ARN** (the specific instances registered for management) + `rds:DescribeDBInstances`. No `rds:Delete*`, no `rds:Create*`.

## 3. Secrets

- All credentials (cloud API keys, DB connection, OIDC client secret) in the cloud Secret Manager / Vault — never in ConfigMaps, never in the repo.
- k8s credentials: `ServiceAccount` token projection (short-lived, auto-rotated).
- Secrets rotation documented in the runbook; the weekly digest includes "last secret rotation" status.

## 4. Supply chain

- Dependencies pinned (go.sum, lockfile) + dependabot with automerge for patch level only.
- Multi-stage Docker build; final image is distroless, non-root (`USER 65532`).
- Trivy scan in CI with **fail on HIGH/CRITICAL**; image signed with cosign; SBOM (syft) attached.
- Helm chart pins image digests, not tags.

## 5. Network

- Consize's engine has **egress allowlist**: Prometheus, k8s API, cloud APIs, Postgres. No general internet egress (network policies on the cluster; security group rules for Postgres).
- UI reachable only via ingress; engine API not exposed externally.
- Postgres: private subnet, TLS required, no public endpoint.

## 6. Audit & tamper resistance

- Every apply/verdict/rollback writes an immutable `apply_event`; the store's writer role has INSERT-only (no UPDATE/DELETE) on audit tables.
- Audit logs also shipped to the cloud log sink (CloudWatch/Cloud Logging) for external retention.
- UI surfaces the audit trail (who/what/when/evidence) — the demo *shows* the audit trail.

## 7. Rate limits & abuse

- API: per-role rate limits (view: 100 rps, operator: 10 rps); apply endpoint additionally limited to 1/min per namespace.
- Apply approval tokens expire in 24 h; re-approval required after policy changes.

## 8. Security checklist (release gate)

- [ ] Trivy: 0 HIGH/CRITICAL on final image; base image current
- [ ] cosign signature verified in CI + deploy gate
- [ ] RBAC review: write SA can prove "cannot touch anything outside explicitly write-enabled namespaces" (assertion test in CI, using `kubectl auth can-i` against a fixture namespace set)
- [ ] Network policies applied to all Consize components
- [ ] Secrets in Secret Manager; none in git history (pre-commit scan + CI scan)
- [ ] Audit table grants verified (INSERT-only)
- [ ] Dependency scan clean; SBOM attached to release
- [x] **Auth roles enforced server-side** — the local-user version of the OIDC contract ships (ADR-037): a viewer session cannot POST apply (403 with `role_required`), actor is server-verified, covered by `TestAuthHandlerMatrix` + `TestActorIsServerVerified` in `engine/internal/api/server_auth_test.go`. The same contract re-runs against the OIDC provider when it lands.

## 9. Incident response notes (for the docs' troubleshooting guide)

- Suspected compromise: revoke engine write SA token + cloud write policy immediately (15-second action, documented), block ingress, then audit `apply_events`.
- The rollback capability itself is the tool: every apply is reversible by design — restoring the previous state is a recorded operation, not a manual scramble.

## 10. Team ownership and escalation (ADR-043)

- Team names, owners, and on-call contacts are operational metadata, not
  credentials. They must not contain API tokens, passwords, or private keys.
- Team creation, contact changes, and workload assignment are admin-only API
  operations; viewers and operators can read ownership but cannot redirect
  incident handling.
- Collector upserts preserve `workloads.team_id`, preventing a metadata refresh
  from silently clearing ownership during an incident.
- The existing shared alert webhook includes the assigned team and on-call
  contact. Per-team external routing remains deferred until its provider
  authentication, delivery semantics, and failure handling can be designed.
