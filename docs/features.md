# Consize — Feature Guide

> Where every feature is, how it works, and exactly where the code lives.

## Overview

Consize is an infrastructure rightsizing platform that finds the waste in your
Kubernetes workloads and cloud databases, removes it safely, and proves the
savings. The product is a loop, not a report: **analyze** real usage →
**recommend guarded applies** → **verify** the change against the system's own
signals → **auto-rollback** on regression → **audit** every step. One dashboard
and one savings number cover both surfaces — compute (pod requests/limits) and
data (database instance classes). The savings figure is the demo; the safety
engine is the product. Two defining principles run through everything: nothing
applies silently, and no apply happens without an audit trail (ADR-008).

---

## 1. Compute surface (collector + Prometheus)

**What it does.** Ingests Kubernetes workload metadata and 14 days of Prometheus
usage for every managed deployment, stored as idempotent 15-minute buckets in
`usage_buckets` (P50=P95=P99=Max per window — single-sample windows, ADR-011).

**How it works.** A one-shot collector binary runs as a CronJob every 15 minutes
(`*/15 * * * *`). It runs Prometheus `query_range` queries for CPU and memory
(`container_cpu_usage_seconds_total` rate, `container_memory_working_set_bytes`)
(`internal/collector/collector.go`), and resolves pod→deployment ownership with
three bulk list calls — never per-pod lookups (ADR-015). `CONSIZE_NAMESPACES`
scopes reads per-namespace so a least-privilege Role works (ADR-025). Windows
are upserted by `(workload, metric, window_start)`, so re-runs are safe and
backfill is a re-run (ADR-007).

**Where the code lives.**
- `engine/cmd/collector/main.go` — binary entry point
- `engine/internal/collector/collector.go` — orchestration, k8s + Prometheus intake
- `engine/internal/collector/k8s.go` — metadata + owner resolution
- `engine/internal/collector/prometheus.go` — `query_range` client
- `engine/deploy/collector-cronjob.yaml` — the CronJob manifest

**Where you see it.** The `consize-collector` CronJob; its output shows in
`GET /api/v1/workloads` and every chart.

## 2. Database surface

**What it does.** Brings cloud databases (RDS today, Cloud SQL next) into the
same model: DB instances are Workloads with `Source="db"`, their metrics ride
the same `usage_buckets` under `db_cpu_percent`, `db_iops`, `db_connections`,
`db_mem_percent`, `db_errors` (ADR-030).

**How it works.** A collector-side `Source` seam (`ListInstances`, `Series`)
keeps cloud adapters thin (ADR-030 §8, ADR-033). The **fixture** (a deterministic
demo source: `rds/payments-prod`, `db.t3.large` → `db.t3.medium`, $50/mo) exists
for tests and demos via `CONSIZE_DBMETRICS=fixture`, but **all live paths run a
real adapter — the demo seed was deleted from the live store** (ADR-035). The
**AWS CloudWatch adapter** implements the seam against RDS (`DescribeDBInstances`
paged, `GetMetricStatistics` chunked to the 1,440-point cap), reusing the
pricing package's SigV4 signer — no AWS SDK dependency (ADR-034). It maps
CPUUtilization, FreeableMemory→mem %, Read+WriteIOPS, and DatabaseConnections;
`db_errors` has no CloudWatch equivalent, so the verifier treats it as
no-evidence, never FAIL. The **GCP Cloud Monitoring / Cloud SQL adapter** (ADR-035)
implements the same seam against `sql/v1beta4` + Monitoring `v3/timeSeries`,
auth via a hand-rolled RS256 JWT from the service-account key (metadata-server
fallback in-cluster). It maps `database/cpu/utilization` and
`database/memory/utilization` (×100, clamped) plus `database/network/connections`;
IOPS and errors have no GCP equivalent → no-evidence, never FAIL. Class catalogs
are **provider-scoped** (RDS classes never cross-recommend GCP tiers); the
maintenance-window day convention is Monday-first per the Admin API (test-locked).

**Where the code lives.**
- `engine/internal/dbmetrics/dbmetrics.go` — the `Source` seam + fixture
- `engine/internal/dbmetrics/cloudwatch/cloudwatch.go` — live RDS adapter
- `engine/internal/dbmetrics/cloudmonitoring/cloudmonitoring.go` — live Cloud SQL adapter (JWT auth, day mapping, metric mapping)
- `engine/internal/analysis/db.go` — `GCPDBCatalog` (provider-scoped, price-ordered)
- `engine/deploy/collector-cronjob.yaml` — `CONSIZE_DBMETRICS` wiring (live cluster: `gcp` + SA key from `consize-gcp` Secret)
- `engine/internal/store/migrations/0003_db_surface.sql` — DB fields

**Where you see it.** DB workloads in `GET /api/v1/workloads`, class
recommendations (`Resource="class"`) in `/recommendations`. On the live cluster
(`devops-portfolio`): `gcp/consize-demo` (`db-custom-1-3840`, us-central1,
window sun 03:00–04:00) ingested from Cloud Monitoring with a real
`db-g1-small` $29/mo recommendation; `consize-collector` runs
`CONSIZE_DBMETRICS=gcp`.

## 3. Analysis engine

**What it does.** Turns buckets into recommendations: per-workload request/limit
targets for compute, and instance-class targets with headroom guarantees for
databases.

**How it works.** Pure functions, golden-tested. Compute: daily p95 series, then
window percentiles — **request = p95 × 1.2**, **limit = max(2×request, p99)**,
downsize-only (ADR-002, ADR-003). Workloads are skipped, with a recorded reason,
when: labeled excluded, in a protected namespace, flagged `data-loss-risk`,
under the data-minimum gate (`CONSIZE_MIN_DATA_DAYS`, default 5 — ADR-024), or
already optimal; half-known windows are dropped in the merge, never built on
half-truths (ADR-016). Databases: a class catalog with utilization caps
(CPU < 60%, IOPS < 60%, mem < 75%, connections < 70% — ADR-030 §9),
cheapest-fit selection, and explicit bottleneck attribution ("kept …
bottleneck X") when nothing fits.

**Where the code lives.**
- `engine/internal/analysis/analysis.go` — compute policy + skip conditions
- `engine/internal/analysis/db.go` — class catalog, caps, candidate search
- `engine/cmd/analyze/main.go` — the CronJob binary
- `engine/cmd/demo/main.go` — the 60-second fixture demo report
- `engine/internal/fixtures/` — the 10 deterministic fixture workloads

**Where you see it.** The `consize-analyze` CronJob (`*/15 * * * *`); output in
`GET /api/v1/recommendations?status=pending`.

## 4. Pricing

**What it does.** Turns sizes into dollars, with degradation as a design
principle: pricing never fails analysis.

**How it works.** Three layers (ADR-014): `Static` (shipped GKE-style default
rates), `AWS` (SigV4 fetch of the EC2 on-demand index, median $/vCPU-hr and
$/GiB-hr × 730 h, 24 h TTL cache), and `Resilient` (falls back to static on any
primary error, with a visible warning). `CONSIZE_PRICING=static|aws` selects
the mode. **GCP pricing is deferred** — same `Service` interface when it lands
(M1 plan). The same package exports the `AWSSigner` the CloudWatch adapter
reuses.

**Where the code lives.**
- `engine/internal/pricing/pricing.go` — Service interface, Static, Cached, Resilient
- `engine/internal/pricing/aws.go` — AWS Price List client
- `engine/internal/config/config.go` — env plumbing

**Where you see it.** The active price table rides in `GET /api/v1/savings` —
a fallen-back table is visible, not silent.

## 5. The safety engine: guarded apply → verify → auto-rollback → audit

**What it does.** The reason Consize is a platform and not a report. Every
change is dry-run-able, guardrailed, verified, and reversible.

**How it works.** The k8s apply engine (`internal/apply`) enforces six
guardrails before any patch: store health (the audit trail must be up —
ADR-008), pending-only, exclusions win, mode policy (`auto` needs the
`consize.savings.dev/auto-apply` namespace label; everything else needs an
actor — ADR-004), a ≤30% step limit, and concurrency (one in-flight apply per
namespace, global cap). Patches go through the `K8sPatcher`: proportional
per-container distribution with exact-sum rounding and QoS-class preservation,
`resourceVersion`-guarded updates with conflict retries — one write surface
shared by apply and rollback (ADR-021). Larger reductions step down 30% at a
time; each remainder materializes as a follow-up pending recommendation that
cannot apply until the previous step verifies — the apply → verify → apply
rhythm by construction (ADR-020).

Verification (`internal/verifier`) is a one-shot CronJob binary (hourly,
ADR-018): it compares a 24 h pre-apply baseline against the post window on
namespace-scoped kubelet-native signals — throttling, OOM kills, restarts,
evictions — with opt-in app-level error/p99 expressions (ADR-019). Verdicts
are three-valued: **passed | failed | inconclusive**. Rollback fires only on
FAIL (ADR-022); inconclusive is terminal, never silent, and never rolls back —
a metrics-path failure must not veto a good change (ADR-027). Rollback restores
**pre-apply values absolutely**, drifting live state included, not the inverted
diff (ADR-026). FAIL also alerts — structured logs always, Slack webhook when
`CONSIZE_SLACK_WEBHOOK` is set (`internal/alert/alert.go`).

The audit trail is INSERT-only: `apply_events` records `planned → applied →
reverted` as new rows, never edits, and in-flight state is derived (an
`applied` event with no `verification_runs` row), so a crash leaves a retryable
trail, not a lie (ADR-023).

Databases get the same philosophy with two extra guardrails
(`internal/dbapply`, ADR-031): a **maintenance window** (weekly UTC, enforced
on every real apply, fail-closed when unconfigured) and **one class step per
apply** (adjacent catalog class only; multi-step moves queue follow-ups).
Approval is the default: `mode=auto` requires
`consize.savings.dev/auto-db=enabled`. DB verification (`verifier/db.go`)
judges store buckets against the **absolute analysis caps on the applied
class** — the threshold is the cap, not baseline × multiplier, because a
healthy downsize legitimately raises utilization (ADR-032). The provider is a
stub (`StubChanger`); real writes fail with an explicit "manual class change
required" until a live provider lands.

**Where the code lives.**
- `engine/internal/apply/apply.go`, `engine/internal/apply/k8s.go` — guardrails, patcher, rollback
- `engine/internal/dbapply/dbapply.go` — DB guardrails, maintenance window, stub changer
- `engine/internal/verifier/verifier.go`, `engine/internal/verifier/db.go` — SLI comparison, DB judgment
- `engine/internal/alert/alert.go` — notifications
- `engine/internal/store/migrations/0002_apply_audit.sql` — `apply_events` + `verification_runs`
- `engine/cmd/verify/main.go` — the verifier CronJob binary
- `engine/deploy/verify-cronjob.yaml`, `engine/deploy/rbac.yaml` — schedule + write identity

**Where you see it.** `POST /api/v1/recommendations/{id}/apply` (dry_run /
approved / auto), `GET /api/v1/applies`, `GET /api/v1/verification-runs`, and
the `consize-verify` CronJob (hourly, off the :00 minute). 503 without a write
identity, structured 422 with reasons when guardrails block — never silent.

## 6. The API

**What it does.** The REST contract behind the dashboard, same origin as the
UI (ADR-029).

| Endpoint | Contract |
|---|---|
| `GET /healthz` | liveness, no dependencies |
| `GET /readyz` | store (and cluster/DB engine, if configured) reachable — gates applies (ADR-008) |
| `GET /api/v1/workloads` | all workloads, k8s and DB |
| `GET /api/v1/workloads/{id}` | one workload, DB fields included |
| `GET /api/v1/workloads/{id}/series?metric=&days=` | chart contract: five metric names (`cpu_percent, mem_percent, iops, connections, errors`), surface-aware units; no-data is 200 with empty points (ADR-034 §3) |
| `GET /api/v1/recommendations?status=&workload_id=&limit=&offset=` | paginated (default 100, cap 500, `pagination.total`), ranked by savings, with `risk` + `risk_reasons` |
| `GET /api/v1/savings` | projected + realized + `by_owner` + active price table |
| `GET /api/v1/system/status` | Consize's own pipeline health: store status, latest telemetry bucket, freshness age, stale threshold, workload/pending counts, in-flight applies, due verifications |
| `POST /api/v1/recommendations/{id}/apply` | `{"mode","actor"}`; routes by resource — class → DB engine, cpu/memory → k8s engine (ADR-031 §6) |
| `GET /api/v1/applies?workload_id=&result=` | the INSERT-only apply trail, newest first |
| `GET /api/v1/verification-runs?apply_event_id=` | verdicts + SLI evidence |

**Where the code lives.**
- `engine/internal/api/server.go` — router + all handlers; `engine/internal/api/savings.go`, `series.go`, `risk.go`, `status.go`
- `engine/cmd/api/main.go`

**Where you see it.** Port 8080 (`CONSIZE_LISTEN_PORT`); the dashboard at `GET /`.

**Self-observability addendum.** The dashboard consumes `/system/status` to show
whether Consize's own collector/analyzer/verifier loop is fresh. The shipped
stale-data threshold is configurable with `CONSIZE_DATA_STALE_AFTER` and
defaults to 2 h — enough tolerance for missed 15-minute collector ticks, but
short enough to catch a broken pipeline before users mistake stale data for a
valid recommendation.

## 7. The UI

**What it does.** One dashboard for both surfaces: savings tiles, ranked
recommendations with risk pills, per-workload 14-day percentile charts, the
apply audit timeline — with the safety loop front and center.

**How it works.** The **product UI is a Next.js app** in the top-level `ui/`
directory (App Router, TypeScript, Tailwind, Recharts; ADR-036) — benchmarked
to usage.ai-class dark FinOps consoles: near-black canvas, sidebar navigation,
KPI cards with dollar deltas, status pills, uppercase micro-labels. It is a
typed read-mostly client: `lib/api.ts` talks to relative `/api/v1`, which
`next.config.ts` rewrites to `API_UPSTREAM` (default `http://127.0.0.1:18099`)
so one build runs against the local poke, the cluster, or a cloud backend —
same-origin, no CORS. No apply buttons by design; RBAC is enforced server-side,
not by hidden buttons. The **embedded vanilla SPA in the API binary** (served at
`GET /`, no build step) remains as the single-binary fallback — untouched, not
developed further (ADR-036).

**Where the code lives.**
- `ui/app/` — routes (dashboard, workloads, workload detail, recommendations, audit, apply)
- `ui/components/` — `Sidebar.tsx`, `ApplyModal.tsx`, `ApplyTimeline.tsx`, `UsageChart.tsx`, primitives
- `ui/lib/api.ts`, `ui/lib/types.ts`, `ui/lib/format.ts` — typed client + API contract types
- `ui/next.config.ts` — `/api/v1` rewrite to `API_UPSTREAM`
- `engine/ui/ui.go` + `engine/ui/app.js` — the embedded fallback SPA

**Where you see it.** `next start` on the poke (against `127.0.0.1:18099`) or
the cluster; the embedded fallback at `GET /` on a running API.

### The polish pass (E1.6–E1.8, ADRs 038–040)

Three UX slices shipped 2026-08-26 as one pass (user's reorder: branding →
navigation → light/dark):

- **Branding (ADR-038).** One `Brand` component is the only logo renderer
  (sidebar, login, loading frames): the gauge brand tile + the **conSize
  wordmark with a capital S in the brand green**. Favicon is a gauge mark
  (`ui/app/icon.svg`), page title "conSize — infrastructure rightsizing".
- **Navigation (ADR-039).** Grouped sections (Overview / Optimize /
  Operations) with active-route marking; a **⌘K command palette** (routes +
  live workload jump by name, lazily indexed on first open); on screens below
  lg the sidebar is an **off-canvas drawer** toggled from a sticky top bar
  (hamburger + compact brand + theme toggle), closed by backdrop, link click,
  or route change.
- **Light/dark mode (ADR-040).** `next-themes` stamps `data-theme` on `<html>`
  (default dark, persisted in localStorage); one `:root[data-theme="light"]`
  token block re-themes the whole console — panels, charts, pills, and the
  brand tile — with the accents re-tinted for contrast on white. Toggle
  surfaces: Dark/Light segmented control in the sidebar footer, icon buttons
  on the mobile top bar and the login page.

**Where the code lives (polish).**
- `ui/components/Brand.tsx`, `CommandPalette.tsx`, `ThemeProvider.tsx`, `ThemeToggle.tsx`
- `ui/app/icon.svg` (favicon), `ui/app/globals.css` (`.brand-tile`, `.nav-section`,
  `.palette*`, `.kbd`, light token block)
- `ui/components/Sidebar.tsx` (sections + drawer), `ui/components/Shell.tsx`
  (drawer state, mobile top bar, palette mount), `ui/app/login/page.tsx` (brand + toggle)

**Where you see it.** Dark is the default; the sidebar's Dark/Light control or
the top-bar icon flips the whole console; ⌘K jumps anywhere from any page.

### The visual standard (ADR-041, 2026-08-26) — Zorveus-standard restyle

The polish-pass look was reviewed against the reference the user set
(zorveus.com — "I want that standard") and rebuilt the same day. The
structural slices above (ADRs 038–040) are unchanged; their **rendering** now
follows the *measured* Zorveus standard (screenshots + computed styles, not
guesswork):

- **Geist** replaces Inter (Vercel's grotesque, `--font-geist` token).
- **Pure-neutral palette** — dark: pure-black canvas `#000000`, `#0a0a0a`
  surfaces, `#141414` cards with a new `--raise` gradient top token so light
  mode re-themes card faces too; light: `#f7f7f7` canvas, white panels
  (superseding the ADR-040 slate palette). White-alpha borders everywhere.
- **Vercel layered shadows**, 14 px card radius, ghost/solid button pairing,
  circular avatars, pill-badge kickers, 34 px KPI values, tighter tracking —
  with **green as the single brand accent** in both themes.

Everything flows from the `:root` tokens, so the restyle touched presentation
tokens plus four small edits (Geist wiring, login hero, two hover classes, KPI
gap) — no component logic or data changes. Full mapping and evidence in
`docs/decisions.md` ADR-041; verified by the polish smoke 33/33 and auth smoke
10/10 with the new pixel values, plus pixel-level screenshot checks against
the token values (`/tmp/ui-smoke/consize-new-{login,dashboard,dashboard-light}.png`).

Same-day amendment (ADR-041 Amendment 1, per the user's clarification "make it
like that standard", not copy it): card radius is now **18 px** through the
`--radius` token (Zorveus's measured card radius, within the announced 14–18
range; modal/palette/safety radii follow the token), the primary CTA stays the
brand green, and the UI source was swept — 53 unnecessary comments removed
(narrative blocks, ADR citations), keeping only the ~50 that carry information
the code can't show (enum values, wire formats, race guards, server contracts).

Second amendment (same day, user directive — the copy "makes the whole thing
look AI-generated"): the five marketing/explainer blocks are gone from the
product UI and **reserved for a future landing page** (plan.md backlog): the
sidebar safety-engine card (incl. the ⌘K hint), the dashboard's 5-step safety
strip and its subtitle, the login "Infrastructure rightsizing" hero pill, and
the "Sessions are server-verified · roles viewer / operator / admin" footnote.
The functional first-run wizard message stays. Dead CSS followed (`.safety*`
strip block, sidebar-only `.kbd`); `PageHead.sub` is now optional and the
dashboard renders title-only. Re-verified: build clean, polish smoke 33/33,
auth smoke 10/10, screenshots re-captured + pixel-checked, and a live-DOM
absence check found none of the five strings on login or dashboard.

Third amendment (same day — the premium-dashboard refinement brief, ADR-042):
flat ultra-dark surfaces replace the layered cards (`#0a0a0a` panels, hairline
white/5 borders, shadows off — superseding ADR-041's `#141414` + Vercel
shadow), the neon green is muted to **emerald `#10b981`** and **reserved for
the active sidebar item + positive financial metrics only**, so the primary
CTA is now solid white and decorative green is swept (login glow, avatar
circle, hovers, palette selection, spinner). Brand lockup is a single-line
extrabold conSize with a minimal emerald diamond mark (no tile, no
RIGHTSIZING sub-text; favicon matches). Ink is pure white, faint is zinc-500;
micro-type is 10 px uppercase tracking-widest; neutral pills are rounded-full
white/10 with white text (semantic pass/fail tints kept); tables are roomier
(th 12/16/10, td 14/16) with right-aligned numerics — white for data, emerald
for savings (`.money`). Verified: build clean, polish smoke **35/35** (new
checks: single-line lockup, white primary, emerald active-nav + savings KPI,
diamond favicon), auth smoke 10/10, screenshots pixel-checked (pure-black
canvas, flat `#0a0a0a` cards, white glow, light `#ffffff` panels).

## 8. Savings semantics

**What it does.** One savings number that can be proven — projected and
realized are never conflated (ADR-034 §4).

**How it works.** Projected = sum of `SavingsMonthly` over **pending**
recommendations. Realized = sum over recommendations whose latest apply event
is still `applied` and whose latest apply has a `passed` verification verdict.
A later `reverted` event, failed verification, or inconclusive verification
excludes it. `by_owner` breaks both down by owner label (unassigned when
absent). On the live cluster today: projected $52.59/mo, realized $0.36/mo
(the one verified apply from the M2 E2E).

**Where the code lives.** `engine/internal/api/savings.go`; realized-eligibility
combines the latest apply event with verification verdicts.

## 9. Risk flags on recommendations

**What it does.** A low | medium | high risk pill with `risk_reasons` on every
recommendation, so the plan ranks by savings and safety.

**How it works.** Computed at the API from existing data — no schema change
(ADR-034 §5): low data days, saturation near the headroom caps, step distance
> 1 class, maintenance window not open, follow-up pending, `data-loss-risk`
flags.

**Where the code lives.** `engine/internal/api/risk.go`; surfaced through
`GET /api/v1/recommendations`.

## 10. RBAC and security posture

**What it does.** Least-privilege end to end: Consize can read only what it
analyzes and update only what it applies, and it dogfoods its own advice
(ADR-010).

**How it works.** Two ServiceAccounts in `engine/deploy/rbac.yaml`:
`consize-writer` — `get/list/watch/update` on **deployments only**, bound
per-namespace via RoleBindings in auto-apply namespaces only, used by both
apply and verify (one write surface, ADR-021); `consize-reader` — read-only on
deployments/replica sets/pods, bound to the analyzed namespaces (ADR-025).
Apply endpoints answer 503 without a write identity. Namespaces opt in with
`consize.savings.dev/auto-apply` / `auto-db` labels; exclusions always win.
Full posture in `docs/security.md` (13/13 RBAC matrix proven live in the E2E).

On top of that sits **user authentication and server-side authorization
(ADR-037)**: local users with roles `viewer` (read-only), `operator`
(approve + apply), `admin` (everything), bcrypt passwords, revocable
Postgres sessions (7-day TTL, hashed tokens), and a login surface
(`POST /api/v1/auth/login`, `/auth/logout`, `GET /auth/me`). Writes are
role-gated server-side — a viewer's apply call answers 403 — and the apply
`actor` in the audit trail is the server-verified session email
(`api:<email>`), never a client-supplied string. `CONSIZE_BOOTSTRAP_ADMIN`
creates the first admin while the users table is empty; the poke runs
auth-enforced, the live cluster runs `CONSIZE_AUTH_REQUIRED=false` until
onboarding lands (E1.5). The provider seam (`internal/auth.Authenticator`)
keeps the door open for OIDC SSO per security.md §2.

Ad-hoc deployments get the interactive equivalent of the bootstrap env var
**(ADR-037 §6 amendment, 2026-08-26): first-run admin creation** —
`POST /api/v1/auth/setup` creates the first admin while `users` is empty
(409 forever after, minimum 8-character password), and `/auth/me`'s 401
body carries `needs_setup:true` so the `/login` page renders a "Create
admin & sign in" wizard instead of the login form. There is deliberately
no default credential (no admin/admin) and no open registration: once one
admin exists, the wizard is gone forever and the only way in is a session.
Verified by `TestFirstAdminSetup` and a 10/10 CDP smoke against a fresh
empty-users stack; the poke's own setup answers 409 (its bootstrap admin
already exists — honest).

**Where the code lives.** `engine/deploy/rbac.yaml`, `docs/security.md`,
`tests/e2e-live/namespace.yaml`, `engine/internal/auth/`,
`engine/internal/store/` (users/sessions, migration `0004_auth.sql`),
`ui/app/login/` (login + first-run wizard), `ui/components/auth.tsx`,
`ui/components/Shell.tsx`.

## 11. Live-cluster E2E and the living demo

**What it does.** Proof the loop works on a real cluster — and a demo that stays
deployed.

**How it works.** `tests/e2e-live/` is a scripted runbook (`run.sh preflight →
deploy → ingest → track1 → verify1 → track2 → verify2 → rbac → status →
summary`) against GKE `devops-portfolio`: Track 1 applies a real
recommendation to `boutique/frontend` and verifies PASS; Track 2 applies to a
synthetic canary, injects a regression, and proves FAIL → auto-rollback →
byte-identical restore. The run surfaced and fixed real bugs: a rollback-drift
defect (ADR-026), the no-baseline conservatism rule (ADR-027), and the durable
SLI storage requirement (ADR-028). Consize stays deployed on that cluster as a
living demo — DB fixture surface included (`CONSIZE_DBMETRICS=fixture`) — so
the whole loop is exercisable end to end at any time.

**Where the code lives.**
- `tests/e2e-live/run.sh`, `tests/e2e-live/teardown.sh`, `tests/e2e-live/namespace.yaml`, `tests/e2e-live/canary.yaml`, `tests/e2e-live/out/`
- Runbook and evidence: `docs/e2e.md`

---

## Architecture

```
                     ┌────────────────────────────────────────────────┐
                     │                Postgres store                 │
                     │  engine/internal/store/  (memory fallback,    │
                     │  migrations 0001–0003, INSERT-only audit)     │
                     └────────────────────────────────────────────────┘
        ▲                                 │        │        │        │
        │ usage_buckets, workloads        │        │        │        │
        │                                 ▼        ▼        ▼        │
┌───────────────┐               ┌───────────────┐   ┌────────────────────┐
│  Collector    │               │   Analysis    │   │  API + embedded UI │
│ cmd/collector │               │  cmd/analyze  │   │  cmd/api           │
│ internal/     │──────────────▶│ internal/     │   │  internal/api      │
│  collector    │               │  analysis,    │   │  engine/ui (GET /) │
│  dbmetrics    │               │  pricing      │   └──────┬─────────────┘
│  (k8s + DB    │               │  (static|aws) │          │ REST (JSON)
│   sources)    │               └───────────────┘          │
└───────┬───────┘                                          ▼
        │ Prometheus / k8s API / RDS / CloudWatch    ┌─────────────┐
        │                                            │  Dashboard  │
        ▼                                            └─────────────┘
┌───────────────────────────────┐        ┌──────────────────────────┐
│  Apply engine (guarded)       │        │  Verifier (CronJob)      │
│  internal/apply (k8s)         │        │  internal/verifier       │
│  internal/dbapply (DB)        │◀───────│  internal/alert          │
│  dry-run → guardrails → patch │ FAIL→  │  baseline vs post SLIs   │
│  → follow-up steps            │ rollback│  → verdict → rollback   │
└───────────────────────────────┘        └──────────────────────────┘
```

Deployed as: `consize-collector` (CronJob 15m), `consize-analyze` (CronJob
15m), `consize-api` (Deployment, serves API + dashboard), `consize-verify`
(CronJob hourly) — manifests in `engine/deploy/`, write identity in
`engine/deploy/rbac.yaml` (ADR-010: Consize runs in the cluster it manages).

## 12. Teams and on-call ownership

**What it does.** Gives every managed workload an explicit human ownership
boundary. A Team has a name, named owner, and on-call contact; workloads show
their assigned team in both the inventory and detail views. The Teams view
shows the workloads each team owns and lets administrators update escalation
contacts.

**How it works.** `teams` is a small durable directory and `workloads.team_id`
is nullable. Ownership is deliberately outside collector input: a collector
refresh updates observed resource state while preserving the admin-selected
team. All signed-in users can read team data; only admins can create/edit teams
or assign/unassign a workload. A failed verification embeds the team and
on-call contact in its existing notification, so the shared webhook is
actionable today without guessing a provider-specific routing format.

**Where the code lives.** `engine/internal/store/` (migration `0005_teams.sql`
and both Store implementations), `engine/internal/api/server.go` (`/teams` and
workload-team endpoints), `engine/internal/verifier/verifier.go` (alert
ownership labels), and `ui/app/teams/` + `ui/components/views/TeamsView.tsx`.

## 13. Incident routing and on-call ownership (in progress)

Each installation will route actionable failures to the organization's
incident-management system and its Slack collaboration channel. The incident
system—not a manually maintained team directory—owns schedules, escalation,
acknowledgement, assignment, and resolution. Consize keeps a durable local
projection so the dashboard can show the current on-call owner, incident
state, provider link, and the change or rollback still required. Alerts use a
stable deduplication key, are safe to retry, and include the installation,
workload, namespace, failed verification signal, proposed change, rollback
state, and dashboard deep link. Slack delivery uses a configured channel and
Slack user/group ID for a real mention; free-form display names are not
treated as routable identities. Provider credentials are referenced from
Kubernetes Secrets. Incident ownership is accountability and does not grant
apply permission; existing server-side RBAC remains authoritative.

**First slice — Grafana-style notification routing (2026-08-28).** The
verifier emits structured alert events for verification failures and rollback
failures. Each event has labels (`alertname`, `severity`, `namespace`,
`workload`, `resource`, `surface`, optional `team`/`oncall`) plus annotations
for the change, rollback state, failed signal, and dashboard link. The alert
router loads `CONSIZE_ALERT_ROUTING`, which mirrors Grafana's model:
notification policies match labels and route to named contact points. Slack
webhook delivery is the first integration and sends Block Kit messages with a
stable dedup key and optional Slack user-group/user mention. If no routing JSON
is configured, the legacy `CONSIZE_SLACK_WEBHOOK` becomes the default Slack
contact point.

**Product configuration UI.** `/alerting` is now a first-class Operations page
for configuring contact points and notification policies. Admins can save the
routing config and send a test notification; viewers can inspect it. The API
stores only the routing metadata in `app_settings` and rejects raw
`webhook_url` values, forcing Slack webhooks to be supplied as Kubernetes
Secret-backed environment variables such as `CONSIZE_SLACK_WEBHOOK`.

Example routing JSON:

```json
{
  "default_contact_point": "ops-slack",
  "contact_points": [
    {
      "name": "ops-slack",
      "integrations": [
        {
          "type": "slack",
          "webhook_env": "CONSIZE_SLACK_WEBHOOK",
          "channel": "#platform-oncall",
          "mention": "<!subteam^S123456>"
        }
      ]
    }
  ],
  "notification_policies": [
    {
      "name": "critical-verification",
      "match": {
        "severity": "critical",
        "alertname": "ConsizeVerificationFailed"
      },
      "contact_point": "ops-slack"
    }
  ]
}
```

### Ownership UI amendment (2026-08-27)

The ownership route no longer exposes a **Create team** form or manual contact
editing. For the installation-per-team model, ownership is deployment
configuration and is displayed read-only until provider-backed onboarding and
on-call routing land. The route is labeled **Ownership** in navigation and
shows installation scope, owner, on-call context, and managed workloads.

**Follow-up correction (2026-08-27):** after review, the interim ownership
route was removed from navigation and `/teams` now redirects to the dashboard.
Workload detail no longer exposes manual ownership assignment. The backend
compatibility surface remains dormant until installation onboarding and the
remaining provider-backed incident projection/webhook work are implemented.

## 14. Weekly savings reports

**What it does.** Gives stakeholders a periodic savings pulse without turning
Consize back into a passive reporting tool. Admins can enable a weekly Slack
digest and choose the default report range. Anyone with read access can
generate an on-demand report for the past 7, 14, or 30 days and download it as
a PDF.

**How it works.** `internal/report` builds the report from the existing source
of truth: pending recommendations, apply events, verification runs, and the
latest telemetry bucket. Realized savings in the report count recommendations
whose apply was verified during the selected period. Pending opportunity comes
from current pending recommendations. Rollbacks and failed/inconclusive
verifications are counted from the audit trail. Delivery uses the configured
Alerting contact point/policy path, so Slack secrets stay in Kubernetes and
only reporting metadata is stored in `app_settings`.

**Where the code lives.** `engine/internal/report/`, `engine/internal/api/reports.go`,
`engine/cmd/report/`, `engine/deploy/report-cronjob.yaml`, and the Next.js
`/reports` console page. API surface:

- `GET /api/v1/reports/config`
- `PUT /api/v1/reports/config`
- `GET /api/v1/reports/savings?range=7d|14d|30d&format=json|pdf`
- `POST /api/v1/reports/send`

The first live rollout used images `api:consize-weekly-report-20260828` and
`report:consize-weekly-report-20260828`; live smoke verified JSON generation,
PDF generation, Slack send-now, and the disabled CronJob no-op path.

## 15. Cloud-waste opportunities and Terraform PR plans

**What it does.** Extends Consize beyond workload rightsizing into common
cloud-cost leaks: unattached storage volumes, idle load balancers, unused NAT
gateways, and stopped instances whose attached resources are still billed.
Findings are shown as cost opportunities with evidence, risk, estimated
monthly savings, and a reviewable next action.

**How it works.** `internal/costscan` defines a provider `Source` seam. The
live GKE deployment runs `CONSIZE_COSTSCAN=gcp` with the same `consize-gcp`
service-account key used by the Cloud SQL collector. The GCP source queries
the Compute Engine API for detached Persistent Disks and stopped VMs whose
Persistent Disks still accrue cost. The fixture source remains for local demos
and tests only. Scan results are upserted into `cost_opportunities`, keyed by
provider/account/region/type/id, so repeated scans refresh evidence without
duplicating rows. Consize does not delete these resources directly. Instead,
an operator can prepare an audited Terraform PR plan/diff from an opportunity,
keeping cloud changes reviewable and avoiding configuration drift.

The Terraform PR workflow is intentionally **not exclusive to cloud waste**.
Normal rightsizing recommendations also support a PR-plan delivery path:
operators can either run a direct Consize apply for non-IaC workloads and
convenience flows, or generate a Terraform PR plan for teams whose repository
is the source of truth. The MVP stores a planned branch/title/body/diff without
requiring GitHub credentials. GitHub configuration is installation-wide, not
recommendation-specific: admins configure a GitHub organization/account,
token environment reference, and the repositories Consize may read/write.
A monorepo is represented as one authorized repository with a root path;
enterprise teams can add multiple repositories. Specific Terraform file and
resource selection happens in the PR workflow today and should later be inferred
from annotations, repo scan, Terraform state, or ownership metadata — not from
the GitHub connection page. When GitHub credentials are present, Consize can
create a branch, commit the Terraform file change, and open a draft PR.
The Terraform target path must point at a concrete file (`.tf` or `.tf.json`),
not a directory. For monorepos, configure the repository root path as the folder
such as `infra/terraform`, then use a file path such as `workloads.tf` or
`infra/terraform/workloads.tf` in the PR workflow.

**Where the code lives.** `engine/internal/costscan/`, `engine/cmd/costscan/`,
store migrations `0007_cost_opportunities.sql` and
`0008_iac_plans_for_recommendations.sql`, API routes in
`engine/internal/api/cost.go`, deploy manifest
`engine/deploy/costscan-cronjob.yaml`, and the Next.js `/cost` console page.
API surface:

- `GET /api/v1/cost-opportunities`
- `POST /api/v1/cost-opportunities/scan`
- `POST /api/v1/cost-opportunities/{id}/iac-pr`
- `POST /api/v1/recommendations/{id}/iac-pr`
- `GET /api/v1/integrations/github`
- `PUT /api/v1/integrations/github`

Current MVP scope: persisted findings, manual scan, daily GCP CronJob, and
Terraform PR generation for cloud waste and rightsizing recommendations. When
the configured GitHub token is available, Consize creates a branch, updates the
mapped Terraform file, and opens a draft GitHub PR if the selected file is a
Terraform file and contains the expected Terraform resource block/values. The
GitHub integration stores metadata only; tokens stay outside Postgres as
Kubernetes Secret-backed
environment variables such as `CONSIZE_GITHUB_TOKEN`. Future hardening:
traffic-backed idle load balancer and Cloud NAT detection, repository ownership
discovery, broader Terraform patching, GitLab PR creation, and snooze/exempt
policy before any automated cleanup.
