# Consize — Implementation Plan

Milestones are ordered to ship a **demoable vertical slice first** (compute surface end-to-end), then the data surface, then production polish. Each milestone lists tasks and acceptance criteria (AC).

**Timebox:** 8–10 weekends of focused work. Each milestone is shippable and demoable on its own.

---

## M0 — Scaffold & CI (Week 1)

The repo must look like a professional product before any feature exists.

**Tasks**
- [ ] Monorepo layout (`infra/`, `engine/`, `ui/`, `deploy/`, `tests/`, `docs/`)
- [ ] Go module + React app (Vite + Tailwind) scaffolding, lint configs (`golangci-lint`, ESLint)
- [ ] GitHub Actions CI: lint → test → build → container build (multi-stage) → Trivy scan → cosign sign
- [ ] Local dev: `docker compose` with engine + Postgres + kind cluster; `Makefile` targets (`make dev`, `make test`, `make e2e`)
- [ ] Terraform bootstrap: provider config, remote state backend (S3/GCS + locking), a placeholder cluster module
- [ ] README skeleton + `docs/` index (already drafted)
- [ ] Changelog + semver discipline from commit 1 (conventional commits)

**AC**
- `make test` green on a fresh clone; CI green on first push.
- Build image passes Trivy with zero HIGH/CRITICAL findings.
- Terraform `plan` runs cleanly against a real account (no resources yet).

---

## M1 — Compute Surface: Analyze (Weeks 2–3) — **DONE 2026-08-24**

**Tasks**
- [x] Collector: Prometheus range queries → `usage_buckets` (idempotent upsert; backfill = re-run, windows are upserted)
- [x] Collector: workload metadata from k8s API (deployments, labels, bulk pod→RS→deployment owner resolution)
- [x] Analysis engine: percentile math (14-day windows, p50/p95/p99/max) — pure functions, golden-tested
- [x] Recommendation policy (request = p95×1.2, limit = max(2×, p99)) + skip conditions
- [x] Pricing service: static defaults + AWS Price List client (SigV4, EC2 on-demand median, 24 h cache, resilient fallback). GCP catalog ingestion deferred — same `Service` interface when it lands.
- [x] `engine-api` v0: chi router, `GET /workloads`, `GET /workloads/{id}`, `GET /recommendations`, `GET /savings`, `/healthz`, `/readyz`; OpenAPI spec deferred to M4 (UI contract)
- [x] Postgres migrations (embedded, idempotent, `cmd/migrate` + auto-migrate on open)

**AC (M1 status)**
- ✅ Golden fixtures: engine tests + `cmd/analyze` pipeline test match hand-computed values.
- ⚠️ Savings within 2% of the *live* catalog: index parsing is unit-tested against synthetic data; live fetch requires AWS creds + network (CI gate) — the parse-to-math path is verified.
- ✅ Skip conditions: excluded label, protected namespaces, data-loss-risk, < 5 days, already-optimal — all tested.
- ✅ API payloads correct (handler tests); OpenAPI spec moved to M4.
- ✅ Extras shipped in M1: `cmd/collector`, `cmd/migrate`, `cmd/analyze`, `cmd/api` all build and run; in-memory store makes the whole pipeline demoable without a cluster; ADRs 011–016 record the M1 decisions.

---

## M2 — Compute Surface: Apply + Verify + Rollback (Weeks 4–5) — **DONE 2026-08-24**

The safety engine — the part that makes Consize a *platform*, not a report.

**Tasks**
- [x] Apply engine: dry-run patch diff, guardrail evaluation (exclusions, step ≤ 30%, auto-apply labels, concurrency)
- [x] k8s write ServiceAccount manifest (`deploy/rbac.yaml`, least-privilege per security.md), resourceVersion-guarded GET→mutate→UPDATE patches with 3× conflict retry
- [x] Verifier: step-scaled baseline/post windows, SLI comparison (throttling, OOMKill, restarts, evictions; app-level error/p99 exprs opt-in)
- [x] Auto-rollback on FAIL + alert (`internal/alert`: structured logs always, Slack webhook when `CONSIZE_SLACK_WEBHOOK` set)
- [x] `apply_events` + `verification_runs` audit trail (migration 0002, INSERT-only events, derived in-flight state)
- [x] `POST /api/v1/recommendations/{id}/apply` (modes: dry_run | approved | auto) + `GET /applies`, `GET /verification-runs`
- [x] Rollout-aware apply: safe for Deployments, blocked for excluded workloads (exclude label, data-loss-risk, protected namespaces); step splits queue follow-up recommendations that apply only after the current step verifies

**AC (M2 status)**
- ✅ Applying a 50%-reduction recommendation does *two* steps, not one (step limit enforced), and step 2 is blocked until step 1 verifies (apply → verify → apply rhythm).
- ✅ A forced regression (sustained throttling / new OOM) triggers rollback with evidence attached (verifier tests: FAIL verdict → inverse patch → `reverted` event → `rolled_back` status; SLI evidence stored in the run).
- ✅ Audit trail shows actor, mode, diff, and verdict for every apply — nothing applies silently (all three event kinds + verification runs round-trip through both store impls).
- ✅ Guardrail matrix (excluded, no-data, protected ns, concurrent-apply rejection, global cap, store-down fail-safe) fully tested against the memory store; the *live-cluster* E2E ran 2026-08-24 on GKE `devops-portfolio` — full runbook and evidence in [docs/e2e.md](e2e.md): Track 1 PASS on `boutique/frontend` (apply #4 → verdict passed → no rollback), Track 2 FAIL → auto-rollback on `consize-canary` (applies #5/#8/#11 → `failed` verdicts with SLI evidence → `reverted` events #6/#9/#12 → byte-identical restore to pre-apply values), RBAC matrix 13/13 least-privilege, audit trail in the live Postgres store. The E2E found and fixed a real rollback drift bug (ADR-026), proved the no-baseline conservatism (ADR-027), and exposed the durable-SLI-storage requirement (ADR-028).
- ✅ Extras shipped in M2: `cmd/verify` CronJob binary, verifier re-tries transient errors on the next tick, apply endpoints answer 503 (never silently pass) without a cluster write identity; ADRs 017–023 record the M2 decisions.
- ✅ M2 debt closed 2026-08-25 (ADR-029): recommendations pagination (`?limit=`/`?offset=`, default 100 / cap 500, `pagination.total` in every response — exercised live, total 418 at rollout); superseded-row pruning after every analyze cycle (`CONSIZE_REC_RETENTION`, default 168 h — applied/verified/rolled_back/pending never pruned); and the first read-only dashboard (`engine/ui/`, vanilla static SPA) **embedded in the API binary** and rolled live (`api`/`analyze:e2e-v2`) — open `GET /` on the running API to see it. Both store impls verified for the new surface (memory + disposable Postgres in the suite).

---

## M3 — Data Surface: DB Sizing (Weeks 6–7)

**Tasks**
- [x] Collector: DB metrics → `usage_buckets` (instance class, replicas, maintenance window) — `dbmetrics.Fixture` source + `CONSIZE_DBMETRICS` wiring shipped (ADR-033); live RDS/GCP adapters deferred

- [x] DB analysis: headroom-constraint candidate search (CPU < 60%, IOPS > 40%, mem > 25%, conns < 70%) — caps resolved in ADR-030 §9, `analysis/db.go`
- [x] One-step-at-a-time rule + "keep with rationale" output — `dbapply.stepPlan` + `cmd/analyze` kept lines
- [x] Apply: maintenance-window enforcement, one class step, approval default (`auto-db` opt-in flag) — `internal/dbapply`, ADR-031
- [x] Verifier: DB SLIs (CPU saturation, connections, error counters); rollback restores previous class — `verifier/db.go`, ADR-032
- [x] UI: DB workloads in the same dashboard (unified surface filter) — workloads tab, class recs, apply modal with step plan + window state (2026-08-25, smoke-verified)

**AC**
- Against a seeded RDS instance with known utilization, the recommended class matches hand-computed results; bottleneck attribution is correct when no candidate fits.
- Apply is refused outside the maintenance window and without approval; evidence logged.
- Rollback restores the previous instance class and marks the event.
- Compute + data surfaces share one dashboard, one savings number.

---

## M3.5 — Live DB Provider: CloudWatch RDS Adapter (2026-08-25)

The fixture is the demo; the adapter is the product. `CONSIZE_DBMETRICS` gains a real provider before the M4 UI ships. Inserted after M3 because the fixture→live intake is the single biggest demo→product gap.

**Tasks** — DONE 2026-08-25 (ADR-034, image `api:consize-m35-m4.2` on devops-portfolio; GCP follow-up ADR-035, image tag `consize-gcp-1` 2026-08-25)
- [x] `internal/dbmetrics/cloudwatch`: RDS `Source` adapter — ListInstances via `DescribeDBInstances` (paginated), Series via `GetMetricStatistics` (CPUUtilization, FreeableMemory→mem %, Read/WriteIOPS, DatabaseConnections); reuses pricing SigV4 (no new AWS SDK dep); `CONSIZE_DBMETRICS=cloudwatch` + region/filter env; `db_errors` has no CloudWatch equivalent → **no-evidence, never FAIL** (verifier semantics, tested both directions)
- [x] Adapter unit tests against canned RDS/CloudWatch responses (httptest, hand-computed mem %); live-account E2E deferred as CI gate (needs AWS creds — same policy as the pricing live fetch, ADR-014)
- [x] API: `GET /workloads/{id}/series` (daily percentile buckets, **surface-aware**: db_* metrics with units percent/iops/connections/errors, k8s raw metrics with millicores/bytes — compute-series gap found on the live cluster and fixed with tests); savings endpoint gains realized (latest apply event still applied + passed verification, reverted/failed/inconclusive never counted) + per-owner breakdown; recommendations gain risk + risk_reasons
- [x] GCP Cloud Monitoring (Cloud SQL) adapter: same `Source` interface — shipped 2026-08-25 (ADR-035): `internal/dbmetrics/cloudmonitoring`, provider-scoped GCP tier catalog, live on the cluster (`consize-demo` → `db-g1-small` $29/mo, real Cloud Monitoring series); fixture retired from all live paths; maintenance-window Monday-first convention found + fixed + test-locked

**AC**
- ✅ Collector with `CONSIZE_DBMETRICS=cloudwatch` ingests a real RDS instance end-to-end against a mocked CloudWatch/RDS API (unit + integration test).
- ✅ Missing metric semantics documented and tested: no `db_errors` data → verifier treats as no-evidence, never FAIL (two tests, both directions).
- ✅ Realized savings never conflated with projected — live cluster: projected $52.59/mo, realized $0.36/mo (the M2 E2E's one verified apply), distinct fields + distinct UI tiles.

---

## M4 — UI + Reporting (Weeks 8–9) — UI sprint DONE 2026-08-25 (image `api:consize-m35-m4.2`); product UI rewritten in Next.js (ADR-036); reporting tasks remain

**Tasks**
- [x] Savings overview: projected vs realized (distinct tiles, never conflated), per team (owner label — `by_owner` table); monthly *trend* chart deferred with the reporting tasks
- [x] Recommendations list ranked by savings, with risk flags (low|medium|high pill + `risk_reasons` tooltip) and rationale
- [x] Workload detail: 14-day percentile chart (canvas p50/p95/p99/max, per-surface metric toggles + units), current vs proposed, apply history timeline
- [x] Apply audit view (timeline: diffs, actors, step numbers, verdicts, SLI evidence); settings deferred (policy/auto-apply changes remain config-side)
- [x] **Product UI rewrite in Next.js** (`ui/`: App Router, TypeScript, Tailwind, Recharts; typed `lib/api.ts` client; `next.config.ts` rewrites `/api/v1` → `API_UPSTREAM`) — benchmarked to usage.ai-class dark FinOps consoles (ADR-036); the embedded SPA stays in the API binary as the single-binary fallback
- [x] **Authentication + server-side authorization (ADR-037)**: local users with roles viewer/operator/admin, revocable Postgres sessions (hashed 32-byte tokens), `POST /auth/login` / `/auth/logout` / `GET /auth/me`, `CONSIZE_BOOTSTRAP_ADMIN` first-admin bootstrap, apply `actor` server-verified (`api:<email>`, client-supplied actor rejected). API: `api.New` variadic `Options{Auth, CookieSecure}` — nil = disabled, all pre-existing tests green. UI: `/login` page, auth context + route guard, sidebar session block + logout, role-gated Apply surface. Verified: engine handler matrix (401 no-cookie / 403 viewer apply / 200 operator), actor-forgery test, CDP smoke 10/10 on the live poke (guard, login, bad-password error, dashboard, logout), cluster API re-deployed with the new image (migration 0004 on the CNPG DB; `CONSIZE_AUTH_REQUIRED=false` keeps the embedded-SPA fallback and curl actor flows functional — flip to true + `consize-auth` Secret to enforce). **First-run admin creation (ADR-037 §6 amendment, 2026-08-26)**: `POST /auth/setup` creates the first admin while `users` is empty (409 forever after, 8-char minimum), `needs_setup` rides me()'s 401, and `/login` renders a wizard instead of the form when no user exists — no default credential, never open registration. Verified: `TestFirstAdminSetup` + CDP wizard smoke 10/10 on a fresh empty-users stack, poke rebuilt + regression 10/10)
- [x] Weekly digest report MVP (CronJob → configured Slack route; admin toggle; JSON/PDF on-demand reports for 7/14/30 days; includes realized savings from verified applies, pending recommendations, rollbacks, verification issues, and telemetry freshness)
- [ ] Grafana dashboards (see observability doc)

**AC**
- Realized savings numbers come from verified applies only; projected vs realized never conflated.
- ✅ UI read-only users cannot trigger applies; RBAC enforced server-side, not just hidden buttons (ADR-037: `RequireRole(operator)` on the apply route; viewer → 403 with `role_required`, actor server-verified; contract tests `TestAuthHandlerMatrix` + `TestActorIsServerVerified`).

---

## M5 — Production Polish (Week 10)

**Tasks**
- [ ] Security hardening pass (see security doc): RBAC review, network policies, secrets, image signing, audit
- [ ] Load test the engine (10k workloads) — analysis under 15 min nightly budget
- [ ] Full documentation pass: README architecture diagram, quickstart, troubleshooting guide
- [ ] Demo script rehearsal (see demo doc) — record the demo video
- [ ] Cleanup: license, CONTRIBUTING, issue templates, release workflow

**AC**
- Fresh-clone → working Consize in under 30 min (documented quickstart).
- Security checklist (docs/security.md) fully green.
- The recorded demo is the README's cover story.

---

## Milestone dependency map

```
M0 ──► M1 ──► M2 ──► M3 ──► M3.5 ──► M4 ──► M5
```

M3 (DB) is independent of M2's apply engine. M3.5 (live DB provider) is the productization of M3's fixture seam; it must land before M4's charts can claim real-data credibility. M4's weekly digest + Grafana remain after the UI sprint.

## Definition of done (project-wide)

1. Everything in this repo is reproducible: fresh cloud account + cluster → deployed Consize (Terraform + Helm).
2. Every apply is verifiable and reversible; audit trail complete.
3. Tests green locally and in CI; security scans clean.
4. Docs read like a product, not a lab: architecture rationale, troubleshooting, demo script.
5. The demo video shows projected savings → real savings with a rollback in the middle.

---

## UI polish pass (2026-08-26) — E1.6 branding, E1.7 navigation, E1.8 light/dark

Closed as one pass per the user's roadmap reorder (ADR-036's UI is the base;
"improve on the UI and logo, then the improved navigation, then toggling of
light and dark mode"). All three slices done 2026-08-26 — ADRs 038 (branding),
039 (navigation), 040 (theming) in `docs/decisions.md`; feature descriptions in
`docs/features.md` §7.

- **E1.6 Branding [x]** — `ui/components/Brand.tsx` (single render path: gauge
  brand tile + conSize wordmark, capital S in brand green), `ui/app/icon.svg`
  gauge favicon, page title, brand tokens (`.brand-tile`). Applied to sidebar,
  login page, loading/signing-in frames.
- **E1.7 Navigation [x]** — grouped sections Overview/Optimize/Operations with
  `aria-current` active marking; ⌘K command palette (routes + lazy workload
  index, ↑↓/Enter/Esc, mobile-safe); off-canvas mobile drawer (sticky top bar,
  backdrop close, close-on-route-change); `lg:pl-[232px]` content inset.
- **E1.8 Light/dark [x]** — next-themes (`data-theme` attribute, dark default,
  localStorage persistence); single `:root[data-theme="light"]` token block
  re-themes everything incl. the brand tile; Dark/Light segmented control in
  the sidebar footer + icon toggles on the mobile top bar and login page.

**Verification (2026-08-26, poke stack :3000 → :18099, real GCP data):**
`npx next build` clean twice (per feature); polish smoke 33/33 — wordmark
checks, favicon fetch, sections + active state, palette open/4 routes/real
workload jump to `/workloads/1` (inference-api)/Esc, 390 px drawer open →
navigate → close, dark default `rgb(11,15,20)` → Light seg `rgb(245,247,250)`
→ reload persists → Dark back, login toggle both ways; auth-smoke regression
re-run 10/10 (login flow, session block, apply surface, logout). Scripts:
`/tmp/ui-smoke/polish-smoke.mjs`, `/tmp/ui-smoke/auth-smoke.mjs`.

Next up per the roadmap: E1.2 Teams + on-call assignment.

**Superseding note (same day):** after review the look was rebuilt to the
Zorveus standard the user set ("all you just did is very poor, look at
Zorveus.com, I want that standard") — **ADR-041** restyles what ADRs 038–040
structured: Geist for Inter, a pure-neutral black/white palette (dark canvas
`#000000`, light `#f7f7f7`), `#141414` Vercel-shadow cards, pill badges,
circular avatars, ghost/solid buttons, green as the single accent, and a hero
login page with an ambient glow. The structural features above (single brand
path, palette navigation, drawer, next-themes) are unchanged; only their
rendering changed. Re-verified: build clean, polish smoke 33/33 with the new
pixel assertions (`rgb(0,0,0)` / `rgb(247,247,247)`), auth smoke 10/10, and
screenshots pixel-checked with Chrome's own decoder against the token values
(decoder control-tested on the known-good Zorveus screenshot). Screenshots:
`/tmp/ui-smoke/consize-new-{login,dashboard,dashboard-light}.png`.

**Copy removal (same day, user directive — "it makes the whole thing look
AI-generated"):** the product UI no longer carries marketing/explainer copy —
five blocks removed: the sidebar safety-engine card (incl. the ⌘K hint), the
dashboard 5-step safety strip and its subtitle ("Rightsizing with a safety
engine — every recommendation is applied, verified, and audited."), the login
"Infrastructure rightsizing" hero pill, and the "Sessions are server-verified
· roles viewer / operator / admin" footnote. `PageHead.sub` is optional now
(the dashboard is title-only). The removed copy is **backlog for a future
landing page** (when one exists): the "Safety engine — every change is
analyzed, guarded, verified, and reversible. Analyze → Guarded apply → Verify
→ Auto-rollback → Audit" explainer incl. the 5 step descriptions, plus the
sessions/roles line and the hero pill. Re-verified: build clean, polish smoke
33/33, auth smoke 10/10, screenshots re-captured + pixel-checked, live-DOM
absence check clean. Tooling note: `Network.clearBrowserCookies` is
unreliable on this Chrome (151) — the page-level helper
`/tmp/ui-smoke/clear-session.mjs` (`Network.deleteCookies` for
`consize_session`) is the reliable pre-smoke cleanup.

**Premium-dashboard refinement (same day, frontend-redesign brief — ADR-042):**
applied the brief's constraints as a third pass over the Zorveus standard:
flat ultra-dark cards (`#0a0a0a` + hairline white/5 border, no gradients/shadows),
pitch-black canvas, muted **emerald `#10b981`** **reserved for the active
sidebar item + positive financial metrics** (the green primary button became
solid white — the accent-reservation rule), single-line extrabold conSize
lockup with a minimal emerald diamond mark (RIGHTSIZING sub-line and squircle
tile gone; favicon matches), pure-white ink, zinc-500 uppercase tracking-widest
micro-type, rounded-full white/10 neutral pills (semantic pass/fail tints
kept), roomier right-aligned tables with emerald savings cells (`.money`),
white login glow. Re-verified: build clean; polish smoke updated and green
**35/35** (new checks: single-line lockup, white primary, emerald active nav +
savings KPI, diamond favicon); auth smoke 10/10; screenshots re-captured and
pixel-checked (`#000000` canvas, flat `#0a0a0a` cards dark / `#ffffff` light,
disabled white primary sampled `#797979` at 45% opacity). Docs: ADR-042 in
decisions.md, features.md §7 third-amendment paragraph.

## E1.2 — Teams and on-call assignment — DONE 2026-08-27

The first ownership vertical slice is closed (ADR-043): durable teams with a
named owner and on-call contact; explicit admin-only team/contact and
workload-assignment APIs; collector-safe ownership preservation; workload and
Teams UI surfaces; and failed-verification/rollback alerts annotated with the
responsible team and on-call target. Store and API tests cover the round trip,
authorization boundary, assignment update, and collector-upsert invariant.
Verified by the complete Go suite and the Next.js Webpack production build.

The next product work remains the unclosed M4 operational reporting tasks:
weekly digest delivery and Grafana dashboards. Per-team provider-specific
notification routing is intentionally a later integration, not a replacement
for the existing shared webhook.

## E1.9 — Incident routing and on-call ownership — IN PROGRESS

Replace the current descriptive on-call contact with installation-scoped
incident routing. Consize should emit one deduplicated incident for a failed
verification or rollback failure, route it to the organization's incident
management service, and publish a collaboration message in the configured
Slack channel. The incident provider remains the source of truth for
escalation, acknowledgement, assignment, and resolution; Slack is the
responder-facing collaboration surface.

Scope for the first slice:

- [x] Grafana-style alert event/router seam: alerts carry labels, notification
  policies match labels, and matched policies send to named contact points.
- [x] Slack webhook contact-point integration with Block Kit payloads and
  backwards-compatible `CONSIZE_SLACK_WEBHOOK` default routing.
- [x] Stable deduplication key per installation/workload/recommendation/failure
  episode so retries and repeated verifier ticks do not page repeatedly.
- [ ] PagerDuty Events API/Jira Service Management adapters behind the same
  event seam.
- [ ] Provider routing references (PagerDuty service/escalation policy, JSM team or
  responder, Slack channel plus user/group ID) stored as installation config;
  credentials are Kubernetes Secret references, never database values.
- [x] Block Kit alert containing the workload, namespace, proposed
  change, rollback state, dashboard deep link, provider incident link, and an
  explicit on-call mention where the provider supplies a Slack user/group ID.
- [ ] Durable local incident projection with provider ID, status, assignee,
  acknowledgement/resolution timestamps, delivery attempts, and last error.
  The dashboard shows who owns the incident and the next required change;
  ownership does not bypass Consize's existing apply RBAC.
- [ ] Inbound provider webhook (with signed verification and replay protection) to
  synchronize assignment, acknowledgement, escalation, and resolution. A
  successful post-rollback verification may close the incident automatically;
  all other closes are explicit and audited.

**First implementation slice (2026-08-28):** `internal/alert` now implements
Grafana-style contact points and notification policies from
`CONSIZE_ALERT_ROUTING`. Slack is the first contact-point integration and emits
Block Kit messages. Legacy `CONSIZE_SLACK_WEBHOOK` still creates a default
Slack contact point when no routing JSON is configured. The verifier now emits
structured, labeled events for verification failure and rollback failure with
stable deduplication keys.

**Product configuration slice (2026-08-28):** Alerting now has a console page
and API (`/api/v1/alerting/config`, `/api/v1/alerting/test`). Configuration is
stored in `app_settings` as routing metadata only; Slack webhook tokens remain
Kubernetes Secret-backed environment variables. The API and verifier manifests
mount `CONSIZE_SLACK_WEBHOOK` from the optional `consize-alerts/slack-webhook`
Secret so the UI can save policy routing without ever seeing the secret value.

Acceptance criteria: a single failed verification creates one provider
incident and one Slack notification; retries update the same incident; the
currently on-call responder is visible and tagged; acknowledgement and
resolution appear in Consize; delivery failures are visible and retryable; no
provider secret is stored in Postgres; and the apply endpoint still enforces
the operator/admin role independently of incident ownership.

**UI direction amendment (2026-08-27):** the E1.2 manual team-creation surface
was removed from the console. The current installation-per-team direction
treats ownership and on-call routing as deployment/provider configuration; the
UI exposes that context read-only until E1.9 is implemented.

**Follow-up correction (2026-08-27):** the interim read-only ownership page was
also removed from navigation, and `/teams` redirects to the dashboard. The
console will not expose an ownership page until installation onboarding has a
real configuration and incident-routing contract to present.

## E1.10 — Self-observability and stale data detection — IN PROGRESS

Build Consize's own health signal before adding more optimization surfaces.
The first slice adds a first-class `GET /api/v1/system/status` contract that
reports store health, latest telemetry bucket, telemetry age, stale threshold,
workload count, pending recommendation count, in-flight applies, and
verification work due. The dashboard renders this as a pipeline status card so
operators can immediately tell whether the collector/analyzer/verifier loop is
fresh or degraded.

Routing direction: use a Grafana-style model for Consize's own platform alerts
— labels route to contact points/notification policies — instead of hardcoding
one Slack channel as product logic. Deployment config may still provide the
default Slack/contact-point reference, but routing should be policy-driven and
provider-owned. Incident ownership remains E1.9's provider-backed workflow.

Current priority order after this slice:

1. Self-observability/stale data detection.
2. Broader cloud-waste surfaces: unattached volumes, idle load balancers,
   unused NAT gateways, stopped-but-billed instances, orphaned resources.
   First MVP slice is implemented behind a `costscan.Source` seam with a
   live GCP scanner for detached Persistent Disks and stopped VM disk cost,
   persisted findings, API routes, and a `/cost` console page. Idle load
   balancer and Cloud NAT detection require traffic-backed checks next.
3. Snooze/exempt policy with required reasons, expiry, and audit.
4. Terraform/IaC PR workflow to avoid configuration drift. First MVP slice
   stores audited Terraform PR plans/diffs for cloud-waste opportunities and
   rightsizing recommendations. The next slice now has an installation-wide
   GitHub integration foundation: admins can configure a GitHub org/account,
   token environment reference, and multiple repositories for monorepos or
   team-owned repos. Rightsizing recommendations and cloud-waste opportunities
   can now create a branch, update the mapped Terraform file, and open a draft
   GitHub PR when a token is available and the file contains the expected
   Terraform resource block/values. GitLab support, repository ownership
   discovery, and broader Terraform patching remain next.
