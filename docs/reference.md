# Consize — Consolidated Reference: ADRs, Mathematics, Terminology

One-file reference for the whole system: every architecture decision record (ADR-001 → ADR-036), every calculation the engine makes (formula + worked example), and the terminology the codebase, docs, and UI use.

- **[docs/decisions.md](decisions.md) remains the canonical ADR source.** This file consolidates the ADRs for one-stop reading; if the two ever disagree, decisions.md wins and this file should be fixed.
- Code paths are `engine/…` relative to the repo root unless stated; the product UI is `ui/`.
- Worked examples labeled **(live)** use real numbers from the production cluster (`devops-portfolio`, project `devops-portfolio-prod`, instance `consize-demo`); examples labeled **(illustrative)** are synthetic numbers chosen for readable arithmetic.
- Last updated 2026-08-25 (sprint state: GCP adapter live, Next.js UI live on the poke; see [plan.md](plan.md), [features.md](features.md)).

---

# Part A — Architecture Decision Records

## ADR-001: Go for the engine, React for the UI

**Status:** Accepted · 2026-08-23

One binary family (collector, analysis, apply, verifier, API) touching k8s and cloud SDKs; a dashboard for charts and audit views. **Decision:** engine in Go (single module, vertical-slice packages, `cmd/` per binary); UI in React + Vite + Tailwind served as static assets behind the API. **Consequences:** best-in-class k8s/cloud SDK ecosystem, static binaries + distroless images, one language for a team of one; the API contract (OpenAPI) is the shared type boundary. *(Superseded in part by ADR-036: the product UI moved to Next.js; the embedded SPA remains as fallback.)*

## ADR-002: Percentile-based sizing over averages

**Status:** Accepted

Averages hide bursts; maxes recommend the wrong size. **Decision:** recommendations derive from **p95 (request sizing)** and **p99 (limit sizing)** over 14 days of 15-minute buckets, with explicit headroom multipliers — no averages, ever. **Consequences:** robust to spikes and weekends; slightly larger requests than average-based sizing — correct, because Consize's job is *safe* savings, not maximal savings. *(Math: Part B §1–§2.)*

## ADR-003: Downsize-only in v1

**Status:** Accepted

Upsizing recommendations are valuable but expand the safety surface. **Decision:** v1 recommends only reductions; upsizing is an explicit future toggle. Throttling/OOM detection still reports evidence — visibility without action. **Consequences:** smaller blast radius, clearer demo story.

## ADR-004: Auto-apply is opt-in per namespace

**Status:** Accepted

Auto-apply is the differentiator ("platform, not report"), but unconditional auto-apply is how trust dies in one incident. **Decision:** auto-apply runs only in namespaces labeled `consize.savings.dev/auto-apply=enabled`; everything else requires explicit approval. Exclusions always win. **Step limit ≤ 30% per apply.** **Consequences:** safe default, trust-by-config; the demo shows both paths. *(DB surface: approval default + `consize.savings.dev/auto-db` label, ADR-031.)*

## ADR-005: Postgres for the store

**Status:** Accepted

**Decision:** managed Postgres (RDS/Cloud SQL) *outside* the managed cluster — Consize never right-sizes its own database. **Consequences:** battle-tested, familiar, trivially backed up; not columnar like ClickHouse — fine at v1 scale (10k workloads × buckets/day).

## ADR-006: Verification via SLI comparison, not chaos

**Status:** Accepted

The rollback decision needs a signal measured from the system, cheap, and defensible. **Decision:** baseline 24 h pre-apply vs 24 h post-apply on error rate, p99, throttling, OOM/evictions (k8s) and CPU saturation/connections (DB); threshold breaches sustained 5 min → **FAIL → automatic rollback**. **Consequences:** no traffic injection; rollback evidence is charts a skeptic accepts; quiet systems may yield INCONCLUSIVE — handled by an explicit verdict, never silence (ADR-022/027).

## ADR-007: Idempotent bucketed collection, backfill-first

**Status:** Accepted

Collectors crash, windows overlap, Prometheus retention varies. **Decision:** upsert by `(workload, metric, window_start)`; bucketing keyed on source timestamp; explicit backfill flag replays history. **Consequences:** safe re-runs, no dupes, easy recovery; confidence scoring uses data volume so sparse windows degrade recommendations, not crash them.

## ADR-008: Apply never runs without an audit path

**Status:** Accepted

The store (audit trail) is the source of truth for "what did Consize do and why". **Decision:** any apply requires a prior `apply_event` row; if the store is unhealthy (`readyz` fails), applies are blocked regardless of cluster health. **Consequences:** fail-safe by construction; audit is a hard dependency, not a feature.

## ADR-009: One repo, one version, one release

**Status:** Accepted

**Decision:** monorepo, semver from commit 1, single release workflow (tag → CI → scan → sign → chart + image). **Consequences:** simple mental model, atomic releases; infra and engine version-locked — accepted at this scale.

## ADR-010: Self-hosted on the cluster it manages

**Status:** Accepted

The product's own story is "run Consize like you'd run Consize-managed workloads". **Decision:** Consize deploys into the cluster it analyzes (separate namespace, network-isolated), with its own Postgres managed separately. **Consequences:** the demo is dogfooding; honest production story, real maintenance pressure — that's the point.

## ADR-011: Single-sample 15-minute windows in v1

**Status:** Accepted · M1 decisions recorded 2026-08-24

The engine's day = 96 buckets math assumes one value per 15-minute window; sub-window sampling costs 15× the Prometheus load for precision daily-p95 aggregation barely uses. **Decision:** the collector queries Prometheus once per metric at 15-minute steps; every window stores P50=P95=P99=Max = the sampled point. Sub-window sampling is a future refinement with no schema/policy changes (columns exist). **Consequences:** one query per metric per cycle; one bad scrape skews one window — bounded because a window is 1/96 of a day's p95.

## ADR-012: Store behind an interface, memory impl for tests

**Status:** Accepted

**Decision:** all engine code depends on the `store.Store` interface only; `Memory` and `Postgres` implement identical semantics (idempotent upserts, supersede-on-create, denormalized reads). The behavior suite runs against both; Postgres integration gated by `CONSIZE_TEST_POSTGRES`. `store.Open` falls back to memory when `DATABASE_URL` is unset — every binary demos zero-config.

## ADR-013: Embedded versioned migrations, run at startup

**Status:** Accepted

**Decision:** SQL files embedded (`//go:embed`), applied in filename order inside a transaction, tracked in `schema_migrations` (idempotent). `store.Open` migrates on startup; `cmd/migrate` exists for initContainers/CI. **Consequences:** fresh and existing deployments both work; no down-migrations — destructive down-migrations are undesirable with the audit-trail principle (ADR-008).

## ADR-014: Pricing degrades, never fails

**Status:** Accepted

The AWS Price List API can be unreachable, slow, or rate-limited — and analysis must still run. **Decision:** three layers — `Static` (shipped defaults), `AWS` (SigV4 fetch of the EC2 on-demand index; median $/vcpu-hr and $/GiB-hr × 730 h, TTL-cached 24 h), `Resilient` (falls back to static on any primary error, with a warning log). The API exposes the active price table alongside savings. **Consequences:** recommendations and savings always compute; a fallen-back table is visible in the payload, not silently wrong. *(Math: Part B §9.)*

## ADR-015: Bulk pod→deployment resolution, not per-pod lookups

**Status:** Accepted

**Decision:** three bulk list calls per cycle — ReplicaSets (RS→Deployment), Pods (pod→RS), Deployments (metadata). Series whose owner doesn't resolve to a listed deployment are dropped with a log line, never attributed to a wrong workload. **Consequences:** O(3) API calls regardless of fleet size; ownership changes are resolved at collection time, consistent with the same-cycle metadata snapshot.

## ADR-016: Half-known windows are dropped in bucket merge

**Status:** Accepted

CPU and memory arrive as separate bucket rows; a window present in only one series would supply a zero for the other, dragging daily p95s toward zero and fabricating "already optimal" recommendations. **Decision:** `cmd/analyze` merges the two series on `window_start` and drops windows missing either metric. **Consequences:** input hygiene is conservative by construction; re-collection (ADR-007) restores dropped windows.

## ADR-017: Limits ride with recommendations; downsize both together

**Status:** Accepted

A CPU request cut without a matching limit cut does nothing; unedited memory limits keep the OOM risk. **Decision:** every recommendation carries `current_limit`/`proposed_limit` alongside request values (equal values = unchanged); the patcher always writes both, per the same step policy. **Consequences:** sizing math grows one dimension (limit = max(2×request, p99), as before, just carried through); follow-ups and rollback carry both values.

## ADR-018: The verifier is a one-shot CronJob binary, not a service

**Status:** Accepted

Verification needs enough post-apply telemetry to prove the change did not
hurt the workload, but a full 24 h wait after every small step makes the
product feel stuck. **Decision:** `cmd/verify` is a batch binary:
`AppliedEventsUnverified()` → for each event whose step-scaled window is due →
compare SLIs → record verdict → roll back + alert on FAIL → exit. The shipped
base window is 1 h and scales by step number: step 1 waits 1 h, step 2 waits
2 h, step 3 waits 3 h, and so on. `CreateVerificationRun` upserts per apply
event, so a retried tick overwrites rather than duplicates. **Consequences:**
first-step feedback is fast, deeper reductions get more observation time, and
the same `verifier.Service` slots into a daemon unchanged if real-time
verification is ever wanted.

## ADR-019: Workload-scoped kubelet-native SLIs, app-level metrics opt-in

**Status:** Accepted

Most clusters lack app instrumentation; the verifier must still work. **Decision:** v1 verifies four kubelet/cadvisor/kube-state-metrics signals scoped to the Deployment that changed — throttling (`container_cpu_cfs_throttled_seconds_total`), OOM kills, restarts, evictions — at 1-minute resolution so "sustained ≥ 5 min" is measurable. Deployment scoping uses namespace plus the generated pod-name shape (`<deployment>-<pod-template-hash>-<pod-id>`), so a noisy sibling workload cannot roll back a healthy apply. Error-rate/p99 expressions opt in via `CONSIZE_SLI_ERROR_EXPR`/`CONSIZE_SLI_P99_EXPR` (rate-wrapped, `sum by (namespace)`), default off until teams provide app-level labels. **Consequences:** zero-instrumentation verification covers the whole fleet; a workload with no data is *inconclusive*, never a pass (ADR-022). *(Math: Part B §6.)*

## ADR-020: Step splits materialize as follow-up pending recommendations

**Status:** Accepted

A 70% reduction needs several 30% applies; the remainder must be actionable, not just logged. **Decision:** after a real apply, the remainder is inserted as a pending follow-up via `CreateFollowUpRecommendation` — which does **not** supersede existing pending recommendations. The follow-up cannot apply until the current step verifies (in-flight guardrail) — the apply → verify → apply rhythm by construction. **Consequences:** overlap with fresh nightly analysis is possible and cleared when the step finishes; visible in the API, never silent. *(Math: Part B §5.)*

## ADR-021: Proportional patch distribution with exact-sum rounding and QoS preservation

**Status:** Accepted

**Decision:** `K8sPatcher` distributes each resource's step across containers proportionally to each container's current request share; the last container absorbs rounding so totals land *exactly* on the proposed values. QoS rule: a container keeps exactly the request/limit fields it already declared — never add or remove a field (that would change its QoS class); containers with neither field are untouched. Writes: GET → mutate → `Update` guarded by resourceVersion, up to 3 conflict retries on a fresh read. **Consequences:** multi-container workloads stay internally consistent and QoS-stable; one write path shared by apply and rollback.

## ADR-022: Inconclusive never rolls back; nothing-measurable is inconclusive

**Status:** Accepted

FAIL triggers a cluster-touching rollback — the highest-stakes decision in the system; missing data must not be able to trigger it, nor be papered over as a pass. **Decision:** verdicts are three-valued — `passed` | `failed` | `inconclusive`; rollback fires only on `failed`. A signal with data in one window but not the other is inconclusive; no signal judgeable at all is also inconclusive. Inconclusive runs are recorded rows; a genuinely unverifiable event stays inconclusive forever, blocking further applies in its namespace until a human looks. **Consequences:** quiet clusters get flagged, not auto-approved; an inconclusive namespace needs human attention before the next apply — the safety posture v1 wants.

## ADR-023: Append-only apply trail; in-flight state is derived

**Status:** Accepted

**Decision:** `apply_events` is INSERT-only (results are new rows: planned → applied → reverted). In-flight state is *derived*: an `applied` event with no `verification_runs` row. A crash between patch and verification row leaves a retryable state, not a corrupted record; the verifier's upsert (`ON CONFLICT (apply_event_id)`) keeps one row per event while allowing a re-run to overwrite a premature inconclusive. **Consequences:** every claim about the trail is provable from the row sequence; the store's DB role is INSERT-only on these tables (security.md).

## ADR-024: The data-minimum is a configurable confidence gate (CONSIZE_MIN_DATA_DAYS)

**Status:** Accepted

The 5-day minimum is a statistical-confidence rule, not a safety rule — the verifier independently protects every apply. A hard-coded 5 blocked new fleets and made the live E2E impossible (ephemeral test Prometheus, no workload with 5 days of history). **Decision:** `MinDataDays` becomes `analysis.Config.MinDataDays` (float, "distinct days with data"), shipped default 5; `Analyze` delegates to `AnalyzeCfg` so existing behavior is byte-identical. `cmd/analyze` reads `CONSIZE_MIN_DATA_DAYS` (default 5) via a new `config.Float` helper. Confidence still scales with data volume, so a lowered minimum never inflates confidence. **Consequences:** operators can trade statistical confidence for cycle speed; shipped default unchanged. The live cluster runs 0.1 as a documented bootstrap (restore 5 once consize-demo holds 5 days).

## ADR-025: Per-namespace collection scoping (CONSIZE_NAMESPACES)

**Status:** Accepted

`CONSIZE_NAMESPACES` ("ns1,ns2") scopes workload listing to named namespaces. Empty keeps cluster-wide discovery. The production posture pairs that empty value with a read-only `consize-reader` ClusterRoleBinding, while direct writes remain separately scoped through per-namespace `consize-writer` RoleBindings. `NewK8sMetadata(kubeconfig, namespaces)` implements this with `listNS()` returning the configured set or `NamespaceAll`. **Consequences:** operators can choose team-scoped collection or cluster-wide discovery without ever granting cluster-wide write. A RoleBinding's RoleRef resolves in the binding's namespace only, so the write Role must exist in every namespace that explicitly opts into Direct apply.

## ADR-026: Rollback restores pre-apply values absolutely (drift-proof)

**Status:** Accepted

The original rollback inverted the apply diff (`Current ↔ Proposed` swap); that's correct only while live state still equals the state the apply left behind. The live E2E proved otherwise: an external actor drifted the workload *during* the window, and a swapped-diff rollback landed on `live + (preApply − applied)` — restoring nothing. **Decision:** `Rollback` reads the live totals (`Patcher.ReadResources`) and patches with an honest `live → pre-apply` diff; proportional distribution lands totals exactly on recorded pre-apply values regardless of drift. The `reverted` event carries that diff. Pre-apply restore — not re-applying the recommendation — because FAIL means the change was harmful, and "undo our change" is the only safe semantic whether the recommendation caused the regression or an external actor did. **Consequences:** drift-proof by construction, unit-tested with a stateful fake patcher (`TestRollbackAfterDrift`).

## ADR-027: Inconclusive is terminal, and that is the safe failure mode (live E2E data loss)

**Status:** Accepted

During the live E2E the ephemeral monitoring Prometheus was recreated mid-run; a due verification evaluated with an empty baseline window, recorded `inconclusive`, and — correctly — no rollback fired. The event was unverifiable forever. **Decision:** `inconclusive` stays terminal for the event it names, and never rolls back: without baseline evidence, Consize cannot distinguish "the change harmed the workload" from "the data disappeared"; rolling back on absent data would let an infrastructure failure veto a good change. The verdict forces *human* attention, not automated action. If an operator restores the baseline, the verifier's upsert lets a manual re-run overwrite the inconclusive row. **Consequences:** honest "unknown" rows instead of fabricated failures; operators with ephemeral Prometheus must expect occasional inconclusive verdicts ("re-run after data returns").

## ADR-028: Verification requires durable SLI history (live E2E, data-loss round two)

**Status:** Accepted

The monitoring Prometheus lost its history twice; round two was root-caused: node autoscaling scale-downs replaced nodes and the emptyDir-backed StatefulSet's WAL died with the old node (three nodes created in one hour). **Decision:** durable SLI storage is a prerequisite for verification, not an optimization — the monitoring Prometheus received a 10 Gi `standard-rwo` PVC via its Prometheus CR (`spec.storage.volumeClaimTemplate`). The verify window is effectively an SLA on the metrics path; Consize itself cannot repair a broken one — `inconclusive` is the honest symptom. **Consequences:** the E2E's canary cycle runs on durable storage; operational guidance: persist and size the SLI store so it outlives node churn.

## ADR-029: Recommendations pagination, superseded pruning, and an embedded read-only dashboard (M2 debt closure)

**Status:** Accepted

Two debts: `GET /api/v1/recommendations` returned everything unbounded (418 rows live at handover), and the product had no surface to show anything. **Decision** (three coupled):

1. **Pagination is server-side and total-aware.** `ListRecommendations` gains `limit, offset` and returns the matching `total` before slicing. Handler defaults `limit=100`, caps at 500, rejects `limit < 1` / `offset < 0` with 400; every response carries `"pagination": {"limit","offset","total"}`. Sorting (savings descending) happens before slicing, so pages compose into a stable global order.
2. **Only superseded rows are ever pruned, and only by age.** `PruneRecommendations(status, cutoff)` runs at the end of every analyze cycle with `CONSIZE_REC_RETENTION` (default 168 h). applied/verified/rolled_back/pending are never pruned; superseded rows are replaceable by construction.
3. **The dashboard is embedded in the API binary** (`package ui`, `engine/ui/`): one Deployment serves JSON API and page on the same origin — no CORS, atomic versioning. Hash-routed; server paths are `/` + assets only; unknown `/api/*` paths stay honest 404s.

**Consequences:** live cluster runs `api:e2e-v2`/`analyze:e2e-v2` with live pagination; the dashboard serves at `/`. Known v1 wart: wire field names are PascalCase Go struct fields (no json tags) — snake_case is a contract change for the M4 API freeze. Charts landed later with the data surface (ADR-034 §3).

## ADR-030: M3 data surface — databases unify into the existing model

**Status:** Accepted

The M3 acceptance criteria demand "one dashboard, one savings number" — a parallel DB surface would fail the milestone by construction. **Decision** (nine parts):

1. **DB instances are Workloads** (`Source="db"`, `Kind="database"`, `Namespace` = provider namespace like "rds" or the pod's k8s namespace). `Workload` gains DB-only fields, empty for k8s: `DBClass`, `DBReplicas`, `DBMaintenanceWindow` (UTC `ddd:hh:mm-ddd:hh:mm`), `DBProvider` (`aws` | `gcp` | `fixture`).
2. **DB metrics ride usage_buckets**: `db_cpu_percent`, `db_iops` (absolute count), `db_connections` (absolute), `db_mem_percent`, `db_errors` (counter for the verifier). IOPS/connections stored absolute, not as percentages — the denominator is the candidate class's catalog baseline, so percent must be computed per-class.
3. **DB recommendations are Recommendations with `Resource="class"`** plus `ClassCurrent`/`ClassProposed`; savings, lifecycle, supersede-on-reanalysis all work unchanged — one savings number falls out for free.
4. **Class diffs ride `Diff`** (new `current_class`/`proposed_class` JSON fields); `apply_events.diff` is JSONB — no schema change.
5. **Class catalog + analysis live in `internal/analysis`** (`db.go`), pure and golden-tested; shipped catalog is RDS-style with documented default rates; overridable via config.
6. **DB apply is its own service (`internal/dbapply`)** with the same guardrails (store health, pending-only, exclusions win, mode policy, concurrency, audit-first) plus two DB-specific ones: **maintenance-window enforcement** (current UTC time must fall inside the instance's window; `now` injected for tests) and **one-class-step** (adjacent class only; larger moves become follow-ups). **Approval is the default**: `mode=auto` requires the instance label `consize.savings.dev/auto-db=enabled`.
7. **DB verification reads the store, not a live provider** — DB metrics are ingested by the collector, so the verifier's DB path reads buckets from the store: one durable source of truth, same verdict semantics (ADR-006/027); SLIs: CPU saturation, connections, error counters; rollback restores the previous class via the same admin interface.
8. **Provider access is a seam, not a dependency** — `internal/dbmetrics` defines the collector-side `Source` interface (`ListInstances`, `Series`) with a deterministic fixture implementation; live adapters deferred (the M1 GCP-pricing precedent).
9. **Headroom thresholds resolved as utilization CAPS on the projected p95** of the candidate class: CPU < 60%, IOPS < 60% ("IOPS headroom above 40%"), memory < 75% ("free memory above 25%"), connections < 70% — all constants in the DB analysis config.

**Consequences:** M3 is a superset of the M2 shapes — no new tables, no parallel API; DB recommendations appear in existing lists with `Resource="class"`; the unified surface filter falls out of existing data. `Workload`/`Recommendation`/`Diff` carry fields meaningless for k8s workloads (documented in the structs).

## ADR-031: DB apply guardrails — maintenance windows, one class step, approval default

**Status:** Accepted

DB writes add failure modes k8s doesn't have: provider operations with their own timing constraints, and absolute writes (the target class names the full target state — unlike k8s diffs, relative to live). **Decision** (six parts):

1. **Maintenance window** (weekly, UTC, `ddd:hh:mm-ddd:hh:mm`), enforced on every real apply. `end < start` wraps past midnight; `end == start` is malformed; empty or malformed window is **fail-closed for every mode**. Dry-runs are exempt from the *timing* guard but still report `InWindow`/`Window`, and still fail closed on unconfigured/malformed windows.
2. **One class step per apply** — each apply moves exactly one adjacent catalog step; the remainder queues as a follow-up pending recommendation (current class = the class just applied; savings = the price difference it will close). Same philosophy as the k8s ≤ 30% rule; downsize-only.
3. **Approval is the default** — `mode=auto` requires `consize.savings.dev/auto-db=enabled`; `mode=approved` requires an explicit actor; `mode=dry_run` records a planned event and touches nothing, and never queues follow-ups.
4. **The store row is not written by the apply engine** — the collector's next sync converges live state; the follow-up carries the applied class forward; rollback needs no live read (the pre-apply class in the apply event is the target, ADR-032 §6).
5. **`ClassChanger` is the provider seam, `StubChanger` the shipped placeholder** — fails every real write with "manual class change required"; FAIL verdicts then escalate to manual intervention instead of silently doing nothing.
6. **The API routes on the recommendation's resource** — `Resource="class"` → DB engine, cpu/memory → k8s engine; read-only API (neither engine) answers 503 for the whole apply surface; a missing engine 503s its kind; guardrail blocks return structured 422 `{"error":"apply blocked","reasons":[...]}`.

**Consequences:** the DB surface cannot be applied accidentally — no window → blocked; outside window → blocked; auto without label → blocked; multi-step move → one step at a time with an audit trail per step.

## ADR-032: DB verification judges against absolute caps on the applied class

**Status:** Accepted

A relative (baseline × multiplier) threshold would false-positive on every healthy DB downsize — a class downsize *legitimately* raises utilization. **Decision** (six parts):

1. **Judgment against the analysis caps, absolutely** — CPU < 60%, connections < 70% of the **applied** class's baseline, sustained per the 5-minute rule, measured in 15-minute collector steps (one bucket at/above a cap = a 15-minute breach). The baseline window still provides evidence (medians) and the one-window-missing inconclusive rule still applies.
2. **`≥` semantics at the cap** — the analysis promise is "projected p95 *below* the cap", so a bucket exactly at the cap is a breach (`dbLongestRun` uses ≥; the k8s path uses strictly-greater).
3. **Connections projected onto the class actually applied**, not the recommended one — judging against the wrong class would pass a regression (1800 connections = 75% of a `db.t3.large` baseline but 56% of `xlarge`).
4. **The error counter is judged per-bucket median, not window totals** — window lengths differ (24 h baseline vs a fresh post window); `post median > baseline median` is the regression signature.
5. **`Verify` dispatches internally** on the event's resource tag — a class event can never be judged by the k8s path or rolled back through the deployment patcher, and vice versa.
6. **Rollback restores the absolute pre-apply class from the apply event** — never a delta, never a live read; the recorded `ClassCurrent` is the complete rollback target even if live class drifted (same rationale as ADR-026). The reverted event records the inverted class pair.

**Consequences:** a healthy downsize passes (utilization rises but stays under the caps it was sized for); real regressions fail and auto-roll-back; no store metrics → inconclusive, never a pass. The DB path needs no Prometheus client — its only data source is the store.

## ADR-033: DB metrics fixture and collector wiring (demo seed)

**Status:** Accepted

**Decision:** `internal/dbmetrics` ships a deterministic `Fixture` source — one RDS-style instance (`payments-prod`, `db.t3.large`, namespace `rds`, provider `fixture`) running hand-computed golden demand (10% CPU, 12.5% memory, 200 IOPS, 300 connections) modulated by daily (±10%) and weekly (±5%) sinusoids evaluated from the Unix clock, so the golden recommendation (`db.t3.medium`, $50/mo) holds exactly; errors constant 2 per bucket so the error SLI passes. Maintenance window `sun:00:00-sat:00:00` UTC (in-window for every moment except Saturday UTC); `consize.savings.dev/auto-db=enabled` set. The collector gains an optional DB surface (`Collector.DB`, nil = k8s only), ingested after the k8s path with the same idempotent upsert semantics. `CONSIZE_DBMETRICS` selects the source in `cmd/collector`: unset/`none` = k8s only (shipped default); `fixture` = demo; unknown values fail at startup. **Consequences:** the full DB loop is exercisable without any cloud account; **retired from all live paths as of ADR-035** (fixture workload deleted from the live store; the fixture stays in code for tests and the zero-config demo). The verifier never touches this seam — it reads the store.

## ADR-034: Live DB provider — CloudWatch RDS adapter and the chart/reporting contract (M3.5)

**Status:** Accepted

The only shipped `dbmetrics.Source` was the fixture — the biggest demo→product gap: a production deployment pointed at a real fleet would ingest **zero** databases. **Decision** (six parts):

1. **`internal/dbmetrics/cloudwatch` implements `Source` against the RDS/CloudWatch APIs**, hand-rolled with the existing SigV4 (exported as `AWSSigner` from `internal/pricing` — no AWS SDK dependency). `ListInstances` pages `DescribeDBInstances` (query protocol, XML); `Series` folds `GetMetricStatistics` (AWS JSON 1.1) into step-aligned `[start,end)` buckets, chunking windows into ≤ 24 h slices for the 1,440-datapoint API cap. Env: `CONSIZE_DBMETRICS=cloudwatch`, `CONSIZE_AWS_REGION` (default us-east-1), optional `CONSIZE_DB_FILTER`, standard AWS credential env vars. Fields without a `Workload` home ride as `consize.savings.dev/*` labels; MultiAZ → `DBReplicas=2`.
2. **Metric mapping is honest**: `db_cpu_percent` ← CPUUtilization; `db_mem_percent` ← 100×(1−FreeableMemory/(catalog GiB×2³⁰)) clamped at 0 (FreeableMemory dips negative on small instances); `db_iops` ← ReadIOPS+WriteIOPS; `db_connections` ← DatabaseConnections. **`db_errors` has no CloudWatch equivalent → no-evidence, never FAIL**, locked by tests in both directions (a healthy verification passes with the errors SLI "unavailable"; a CPU-saturation regression still FAILs without it).
3. **`GET /api/v1/workloads/{id}/series?metric=&days=` is the chart contract.** Five metric names regardless of surface (`cpu_percent, mem_percent, iops, connections, errors`; anything else 400; unknown workload 404; `days` positive int, default 14). Resolution is **surface-aware**: DB workloads read the `db_*` store metrics with units percent/iops/connections/errors; compute workloads read k8s raw metrics (`cpu_used_milli`, `mem_used_bytes`) with units millicores/bytes. A contract-valid name with no store metric on the surface is **200 with empty points — no-data, not an error**. The response carries `unit`. *(The initial implementation mapped only DB metrics — found live: compute series came back empty while analysis had the buckets; the fix is the surface-aware resolution, tested for both surfaces.)*
4. **`GET /api/v1/savings` gains realized numbers**, additive to the existing projected fields: `realized_monthly`/`realized_yearly` = sum of `SavingsMonthly` over recommendations whose **latest** apply event is still `applied` and whose latest apply has a `passed` verification verdict. A later `reverted` event, failed verification, or inconclusive verification excludes it; projected and realized never mixed. `by_owner` gives the same two numbers per owner label (unassigned when absent). This keeps the M4 AC literal: realized means verified, not merely attempted.
5. **Recommendations gain `risk` (low|medium|high) + `risk_reasons`**, computed at the API from existing data (no schema change): low data days, saturation near headroom constraints, step distance > 1 class, maintenance window not open, follow-up pending, data-loss-risk flags. The UI renders a risk pill with reasons as tooltip, ranked by savings.
6. **GCP Cloud Monitoring (Cloud SQL) is the documented follow-up** on the same `Source` seam — one interface, a second implementation.

**Consequences:** a production deployment with AWS credentials ingests a real RDS fleet end-to-end with the same safety semantics as the fixture. The live cluster as of 2026-08-25: "projected $52.59/mo, realized $0.36/mo from the one verified apply". The `db_errors` gap is documented, not silent.

## ADR-035: Live DB provider — GCP Cloud Monitoring (Cloud SQL) adapter, provider-scoped catalogs (M3.5 §6 closed)

**Status:** Accepted

The user's product focus moved to their real GCP account (2026-08-25 mandate: "don't use seeded data again, I want to focus on the GCP account") — the adapter shipped before the live-account AWS gate, and the fixture demo path was retired from the live cluster entirely. Two facts drove decisions: Cloud SQL only accepts `db-custom-*` or shared-core tiers for Postgres (legacy `db-n1-standard-*` → HTTP 400, learned live), and the Admin API's maintenance-window day convention is **1 = Monday … 7 = Sunday** (verified against `gcloud sql instances describe`: a Sunday window returns `day: 7`). **Decision** (four parts):

1. **`internal/dbmetrics/cloudmonitoring` implements `Source` against the Cloud SQL Admin API and Cloud Monitoring.** `ListInstances` pages `sql/v1beta4/projects/{p}/instances` (JSON, state RUNNABLE only); `Series` queries `v3/projects/{p}/timeSeries` mapped to the five `db_*` store metrics. Auth is hand-rolled: an RS256 JWT minted from the `GOOGLE_APPLICATION_CREDENTIALS` service-account key (RFC 7523, `token_uri`), with the GCE metadata-server fallback for in-cluster runs; `tokenFunc` injectable for tests. Env: `CONSIZE_DBMETRICS=gcp`, `CONSIZE_GCP_PROJECT` (default: key's `project_id`, then metadata), optional `CONSIZE_DB_FILTER`.
2. **Metric mapping follows Cloud Monitoring's surface**: `db_cpu_percent` ← `cloudsql.googleapis.com/database/cpu/utilization` ×100 clamped 0..100; `db_mem_percent` ← `database/memory/utilization` ×100; `db_connections` ← `database/network/connections`; **`db_iops` and `db_errors` have no Cloud Monitoring equivalent → no-evidence, never FAIL**. Series requests carry the RUNNABLE filter, paginate with `nextPageToken`, and map days-to-window with the verified Monday-first convention (`gcpDays`); the initial Monday-first bug (shipped mapping 1=Sunday → a Sunday window rendered `sat:03:00`) was found live on consize-demo, fixed, and the mapping table now encodes the real convention. ZONAL→1 / REGIONAL→2 replicas; userLabels + derived `consize.savings.dev/{region,tier,storage}` labels, derived set last so they cannot be clobbered.
3. **DB class catalogs are provider-scoped.** `analysis.DBCatalog` (RDS) and the new `GCPDBCatalog` (price-ordered: db-f1-micro $11, db-g1-small $43, db-custom-1-3840 $72, db-custom-2-7680 $144, db-custom-4-15360 $288, db-custom-8-30720 $575 — documented us-central1 defaults; `VCPU`/`MemGiB` now float64 for GCP's fractional shapes) are **never merged**: a single ladder would let RDS propose GCP classes and vice versa. `DBClassStep`, `dbapply.stepPlan`/`classPrice`, the verifier's `dbClassFor`, and `dbSizing` all resolve within the workload's own provider's catalog; golden tests lock both directions.
4. **The collector's `CONSIZE_DBMETRICS` switch gains `gcp`** (`dbSourceFor` refactor makes the switch unit-testable). The live cluster's collector CronJob runs `CONSIZE_DBMETRICS=gcp` with the service-account key mounted from a `consize-gcp` Secret (`GOOGLE_APPLICATION_CREDENTIALS=/etc/consize-gcp/key.json`); analyze runs `CONSIZE_MIN_DATA_DAYS=0.1` as a documented bootstrap for the fresh instance (restore default 5 once it holds 5 days). The fixture workload was deleted from the live store — the demo seed no longer exists in any live path.

**Consequences:** the live cluster ingests the user's real Cloud SQL instance end to end: consize-demo (`db-custom-1-3840`, ZONAL, window sun 03:00–04:00, us-central1) → real Cloud Monitoring series → recommendation `db-g1-small` $29/mo with honest risk (medium: low data days, window not yet open). Projected savings are now 100% real: $31.59/mo projected (incl. the $29 GCP rec) + $0.36 realized from the verified M2 apply. A wrong window would have let applies through on the wrong day — the Monday-first convention is locked by tests. Remaining on this seam: the live-account AWS E2E as CI gate (unchanged, ADR-034 §6).

## ADR-036: Next.js product UI (M4 rewrite) — product-standard architecture, embedded SPA retained as fallback

**Status:** Accepted

The M4 UI shipped as a vanilla static SPA embedded in the API binary — demoable, but not product-standard: no component model, no SSR, no typed API layer. The 2026-08-25 mandate: "why are we not using nextjs? … make the UI standard, have you seen usage.ai?" — benchmark: a dark FinOps console (near-black canvas, sidebar navigation, KPI cards with dollar deltas, status pills, uppercase micro-labels). The engine API is already a clean `/api/v1` contract (ADR-034 §3), so a modern frontend can sit on it without engine changes. **Decision** (four parts):

1. **`ui/` is a Next.js (App Router, TypeScript, Tailwind) application** at the repo root, replacing the embedded SPA as the product UI: `lib/api.ts` typed client against relative `/api/v1`, `lib/types.ts` API types, `lib/format.ts` currency/number formatting, `components/` (Sidebar, ApplyModal, ApplyTimeline, UsageChart, ui primitives), `app/` routes (dashboard, workloads, workload detail, recommendations, audit, apply). Charts: Recharts; icons: lucide-react.
2. **`next.config.ts` rewrites `/api/v1/:path*` → `API_UPSTREAM`** (default `http://127.0.0.1:18099`) so the same build runs against the local poke, the cluster, or a cloud-deployed API — the frontend never hardcodes a backend origin; `NEXT_PUBLIC_API_BASE` is the escape hatch. CORS is avoided entirely by same-origin proxying.
3. **The embedded SPA stays in the API binary untouched** as the single-binary fallback (zero-dependency demo: one image, one port); not developed further — new UI work lands in `ui/`.
4. **Deploy story**: `ui/` builds to a static export or Node server image alongside the engine images; the cluster serves it through the same `consize-api` Service (route `/` → UI, `/api/v1` → engine) once the rollout lands; until then the poke runs `next dev`/`next start` against the local API.

**Consequences:** a standard stack a hiring-team reviewer would expect (Next.js, App Router, typed API client), matching the usage.ai-class visual benchmark, sharing one API contract with the embedded SPA. Cost: a second deployable and a build step; the engine remains the single source of truth for all data and safety decisions (the UI is a read-mostly client; applies still require the engine's guardrails).

## ADR-037: Authentication and server-side authorization — local users, revocable sessions, role-gated writes

**Status:** Accepted

The M4 AC ("UI read-only users cannot trigger applies; RBAC enforced server-side, not just hidden buttons") had no identity layer to build on: the apply `actor` was a self-reported JSON body string, `mode=auto` needed no identity, and security.md §2's OIDC promise was aspirational. **Decision** (seven parts):

1. **`internal/auth` package on a provider seam** (`Authenticator` interface) — the LocalUsers provider ships now (bcrypt hashes in Postgres); OIDC implements the same seam later, the repo's signature seam-first pattern (dbmetrics.Source, ClassChanger).
2. **Sessions are Postgres rows, not JWTs**: 32-byte random tokens stored as SHA-256 hashes only, 7-day TTL, revocable (delete on logout), deleted on lookup when expired — no dependency, no client-side state.
3. **Roles `viewer|operator|admin`** (CHECK-constrained in `users.role`), mapped to security.md §2's consize:view / consize:operator / consize:admin; `RequireUser` → 401 `{"error":"unauthorized"}` on reads, `RequireRole("operator")` → 403 `{"error":"forbidden","role_required":…}` on writes.
4. **`api.New` takes `Options{Auth, CookieSecure}`** — zero options = auth disabled; the nil `*auth.Service` is explicitly converted to a nil `Authenticator` interface (the Go typed-nil trap would otherwise have enforced auth anyway), so all pre-existing tests stay green untouched.
5. **Apply actor is server-verified**: the client-supplied body actor is rejected; the trail records `"api:<session user email>"` — the store comment `Actor // operator | auto | api:<user>` is now literally true.
6. **Bootstrap**: `CONSIZE_BOOTSTRAP_ADMIN="email:password"` creates the first admin only while the users table is empty; the UI keys off `GET /auth/me` (`auth_enabled` distinguishes "login required" from "auth not enforced").
7. **Deployment nuance**: poke runs auth-enforced with its bootstrap admin; the live cluster ships `CONSIZE_AUTH_REQUIRED=false` — the embedded-SPA fallback and curl-actor flows keep working, and flipping to true (+ a `consize-auth` Secret) enforces the same contract against the same store.

**Consequences:** the M4 AC is closed (contract tests `TestAuthHandlerMatrix` + `TestActorIsServerVerified`); migration 0004 adds `users` + `sessions`; security.md §2 is no longer aspirational for humans. Remaining seam work: the OIDC provider, rate limits (security.md §7), and API keys for the Terraform PR flow (E2.4) — all ride the same `Authenticator`/middleware seams. *(Amendment 2026-08-26, §6: first-run admin creation — `POST /auth/setup` + `needs_setup` in me()'s 401, wizard on /login; no default credential, never open registration; see the amendment in decisions.md.)*

## ADR-038: Branding — conSize wordmark, brand mark, favicon

**Decision:** one `ui/components/Brand.tsx` is the only logo renderer (brand tile = gauge icon on a token-derived panel with a green ring; wordmark "conSize" with a capital S in the brand green); gauge favicon `ui/app/icon.svg`; title "conSize — infrastructure rightsizing". The tile derives from palette tokens, so light mode re-themes it. *(Full ADR in decisions.md.)*

## ADR-039: Improved navigation — sections, ⌘K palette, mobile drawer

**Decision:** grouped sidebar sections (Overview / Optimize / Operations) with `aria-current` active marking; `CommandPalette.tsx` (⌘K / Ctrl+K, ↑↓/Enter/Esc, routes + lazily-indexed live workloads with source hints); below `lg` the sidebar is an off-canvas drawer owned by Shell (sticky top bar toggle, backdrop + link-click + route-change close, content `lg:pl-[232px]`). *(Full ADR in decisions.md.)*

## ADR-040: Light/dark mode — next-themes, attribute-switched tokens

**Decision:** `next-themes` stamps `data-theme` on `<html>` (default dark, persisted in localStorage, no system-following); one `:root[data-theme="light"]` block redefines every raw token (backgrounds, inks, borders, accents re-tinted for white) so the whole console — panels, charts, pills, brand tile — re-themes with zero component changes; toggles: Dark/Light segmented control (sidebar footer) + icon buttons (mobile top bar, login page). *(Full ADR in decisions.md.)*

---

# Part B — Mathematical Calculations

All formulas below are read from the engine source (`engine/internal/analysis/analysis.go`, `engine/internal/analysis/db.go`, `engine/internal/apply/apply.go`, `engine/internal/dbmetrics/dbmetrics.go`, `engine/internal/verifier/{verifier,db}.go`, `engine/internal/api/{risk,savings,series}.go`, `engine/internal/pricing/aws.go`). Constants are named exactly as in code.

## §1 Percentile — linear interpolation

The engine never uses "the Nth percentile = value at position ⌈N/100×count⌉" rounding. It interpolates:

```
r    = p/100 × (n − 1)          # rank
i    = floor(r)                 # lower index
f    = r − i                    # fraction
P(p) = v[i] + f × (v[i+1] − v[i])   # linear interpolation between sorted neighbors
```

where `v` is the sorted ascending series of n values. **Worked example (illustrative):** n = 14 daily p95s, sorted: `[100, 110, 120, 130, 140, 150, 160, 170, 180, 190, 200, 210, 220, 230]`.

- p95: r = 0.95 × 13 = 12.35 → i = 12, f = 0.35 → v[12] = 220, v[13] = 230 → P95 = 220 + 0.35 × 10 = **223.5**
- p99: r = 0.99 × 13 = 12.87 → i = 12, f = 0.87 → P99 = 220 + 0.87 × 10 = **228.7**

## §2 Compute sizing (request & limit) — `sizeCPU`/`sizeMemory`

**Input aggregation:** the 14-day window of 15-minute buckets is first reduced to one **p95 per UTC day** (`dailyP95`: group by `WindowStart/86400`, take the p95 of the day's samples), then the p95/p99 is taken **over the daily p95s** (see §10 for the chart variant).

```
request  = ceil(p95 × Headroom)        Headroom = 1.2
limit    = max(ceil(p99), request × MinLimitMult)      MinLimitMult = 2.0
limit    = min(limit, current limit)   # limits only decrease in v1
skip if  request ≥ current request     # downsize-only
```

**Worked example (illustrative):** daily p95 series as in §1; current request 500 m, current limit 1000 m.

- request = ceil(223.5 × 1.2) = ceil(268.2) = **269 m**
- p99 = ceil(228.7) = 229 m; request×2 = 538 m → limit = max(229, 538) = **538 m** (≤ 1000 ✓)
- CPU savings: (500 − 269) / 1000 × $27.40 = **$6.33/mo** (§3)

**Note the semantics:** the 1.2× headroom means the p95 sits at 100/1.2 ≈ **83.3% of the request** — that boundary is what the risk scorer references ("the 1.2x headroom boundary is 83%").

## §3 Compute savings

```
CPU savings   = (RequestCPU − request) / 1000  × CPUPerCoreMonth     # Δ cores × $/core-month
Memory savings = (RequestMem − requestGiB) / GiB × MemPerGiBMonth     # Δ GiB × $/GiB-month
```

Prices: `DefaultPrices()` = **CPUPerCoreMonth $27.40, MemPerGiBMonth $3.66** (shipped static defaults, ADR-014). Savings scale with the **request** reduction only (limits don't gate the bill directly).

**Worked example (illustrative, continuing §2):** 500 m CPU → 269 m: 0.231 core × $27.40 = $6.33/mo; memory with a daily-p95 of 466 MiB: reqMiB = ceil(466×1.2) = 560 MiB → 1024 − 560 = 0.453 GiB × $3.66 = $1.66/mo; combined ≈ **$7.99/mo**.

## §4 DB sizing — projections, caps, candidate search — `dbProject`/`dbSizing`/`dbFails`

Observed (window p95s of the DB metrics) are **projected onto the candidate class**:

```
cpu   = obsCPU%   × cur.VCPU    / cand.VCPU
iops  = obsIOPS   / cand.MaxIOPS × 100
mem   = obsMem%   × cur.MemGiB  / cand.MemGiB
conns = obsConns  / cand.MaxConns × 100
```

The candidate fits iff all four are **below the caps** (breach at ≥):

```
DBCPUCap   = 60.0   # projected CPU % must be below this
DBIOPSCap  = 60.0   # projected IOPS % below this
DBMemCap   = 75.0   # projected memory % below this
DBConnsCap = 70.0   # projected connections % below this
```

**Search** (`dbSizing`): downsize-only, candidates = the current class's **own provider catalog** (RDS or GCP, never crossed — ADR-035 §3), strictly cheaper than current, **cheapest first**; the first fit is the maximum-savings class that keeps every promise. If the current class itself fails its own caps, no cheaper class can — the bottleneck attribution names the first saturated dimension in policy order (cpu, iops, mem, conns).

**Worked example (live):** consize-demo, `db-custom-1-3840` (1 vCPU, 3.75 GiB, $72/mo) with observed CPU ≈ 10.4% (p95 over real Cloud Monitoring series):

- candidate `db-g1-small` (1 vCPU, 1.7 GiB, $43/mo): cpu = 10.4 × 1/1 = **10.4%** < 60 ✓
- memory projection scales with the GiB ratio: mem = obsMem% × 3.75/1.7 ≈ obsMem% × 2.21 — the class that wins on price keeps memory headroom only if observed memory is well under 34% (34 × 2.21 ≈ 75).
- savings = $72 − $43 = **$29/mo** (the live recommendation; total projected $31.59/mo includes it).

**Worked example (fixture, golden-tested):** payments-prod `db.t3.large` (2 vCPU, 8 GiB, $100/mo) at base demand 10% CPU, 12.5% mem, 200 IOPS, 300 conns → `db.t3.medium` (2 vCPU, 4 GiB, $50/mo): cpu = 10 × 2/2 = 10% ✓; mem = 12.5 × 8/4 = 25% ✓; iops = 200/1200×100 = 16.7% ✓; conns = 300/800×100 = 37.5% ✓ → **$50/mo**.

## §5 Apply stepping — `stepToward`/`stepValues`/`savingsOf`

**Step rule (k8s):** each apply moves at most **30% of the current value** (`StepLimit = 0.30`):

```
step = floor(current × 0.30)
next = current − step;  if next < target → next = target    # final step lands exactly
if target ≥ current → current                                # upsize/equal: no-op (downsize-only)
```

**Worked example (illustrative):** 1000 m → 100 m:
700 → 490 → 343 → 241 → 169 → 119 → **100** (7 applies; `totalSteps` counts via `for c := req; c != target && c > 0; c = stepToward(...)`).

Each step's follow-up recommendation carries the remainder, with savings **scaled by the request-reduction share**:

```
followUp savings = total × (stepReq − finalReq) / (curReq − finalReq)
```

E.g. total $18.00, stepReq 700, final 100, current 1000: after step 1, follow-up savings = 18 × (700−100)/(1000−100) = 18 × 0.667 = **$12.00**. The follow-up is blocked by the in-flight guardrail until the current step verifies (ADR-020) — the apply → verify → apply rhythm.

**DB stepping** is by catalog position, not percentage: exactly **one adjacent class per apply** (`DBClassStep`), larger moves queue follow-ups (ADR-031 §2); follow-up savings = adjacent class price − final class price (from the provider-scoped catalog).

## §6 Verification math — k8s path — `verifyK8s`/`longestRun`

Baseline window vs post window uses the effective step-scaled duration at
1-minute samples. Each signal has a multiplier; threshold =
**baseline × mult**:

| signal | kind | mult |
|---|---|---|
| throttling (`container_cpu_cfs_throttled_seconds_total`) | rate | 1.0 |
| oom_killed | counter | 0 (any new event) |
| restarts | counter | 0 (any new event) |
| evictions | counter | 0 (any new event) |
| error_rate (opt-in expr) | rate | 1.5 |
| p99_latency (opt-in expr) | rate | 1.3 |

`longestRun(post, threshold)` = the longest streak of consecutive post samples above the threshold; a breach is `run.minutes ≥ SustainedMinutes` (default **5**, configurable via `CONSIZE_SUSTAINED_MINUTES`; E2E used 3). A FAIL fires the automatic rollback + alert.

**Worked example (illustrative):** baseline throttling rate 2.0/s → threshold 2.0/s. Post window has 9 consecutive samples ≥ 2.0/s → 9 min ≥ 5 → **FAIL**.

## §7 Verification math — DB path — `VerifyDB`/`dbLongestRun`

Judged against **absolute caps on the class actually applied** (ADR-032), not baseline-relative:

- `cpu_saturation` ← `db_cpu_percent`, breach when a bucket ≥ **60** (`DBCPUCap`) — `dbLongestRun` uses **≥** (the k8s path uses strictly-greater); buckets are 15 minutes, so one bucket at/above the cap is a 15-minute breach.
- `connections` ← `db_connections` **projected onto the applied class's baseline**: `value / appliedClass.MaxConns × 100 ≥ 70` (`DBConnsCap`). The baseline window still supplies medians as evidence, and one-window-missing → inconclusive (ADR-006/027).
- `errors` ← `db_errors`: per-bucket **median** post > baseline median → FAIL (window-length-agnostic, ADR-032 §4).
- **No-evidence, never FAIL**: `db_iops`/`db_errors` have no CloudWatch/Cloud Monitoring equivalent (ADR-034 §2, ADR-035 §2) — a healthy verification passes with those SLIs "unavailable"; a CPU-saturation regression still FAILs without them.

## §8 Risk scoring — `riskFor`/`nearHeadroom`/`windowP95`

Levels: **low** (default) < **medium** < **high**; the highest flag wins, all reasons are listed in `risk_reasons`. The window statistic used is `windowP95` = the p95 of each day's p95s, then the p95 over the daily p95s.

- **high**: workload flagged `consize.savings.dev/data-loss-risk=true`; malformed maintenance window.
- **medium**:
  - low data days: `daysWithData < MinDataDays` (default 5; reason reads "low data days (N of 5 required)").
  - compute: utilization p95 at ≥ 80% of the current request — the 1.2× headroom boundary sits at 83%.
  - class step spans > 1 catalog class (applies move one adjacent step).
  - no maintenance window configured (class applies are blocked); or window not yet open.
  - pending follow-up of a multi-step plan (`ClassCurrent ≠` the instance's live class).
  - any dimension's window p95 **within 10 points of its cap** (`nearHeadroom`: `p95 ≥ cap − 10`, with absolute counts scaled to percent of the class baseline first): "cpu p95 at 55.2% within 10 points of the 60% headroom cap".

**Worked example (live):** consize-demo → db-g1-small rec shows **medium** with reasons "low data days (1 of 5 required)" (bootstrap at `CONSIZE_MIN_DATA_DAYS=0.1` still reports against the *default* 5 in the reason string) and "maintenance window not yet open" (window sun 03:00–04:00; applies outside it are blocked).

## §9 Pricing conversions — `pricing/aws.go`

```
CPUPerCoreMonth = median($/vcpu-hr across the EC2 on-demand index) × 730
MemPerGiBMonth  = median($/GiB-hr) × 730
```

730 = `HoursPerMonth` (the average month, 365×24/12). The AWS live fetch uses these; the static `DefaultPrices()` ($27.40 / $3.66) is the shipped fallback table (ADR-014). (Golden test against canned AWS data: 35.04 CPU / 8.76 mem — that's the test fixture, not the default.)

**Class catalogs (ADR-030 §5, ADR-035 §3)** — both price-ordered, cheapest first; never merged across providers:

| RDS (`DBCatalog`) | VCPU | GiB | MaxIOPS | MaxConns | $/mo |
|---|---|---|---|---|---|
| db.t3.micro | 1 | 1 | 300 | 200 | 15 |
| db.t3.small | 2 | 2 | 600 | 400 | 25 |
| db.t3.medium | 2 | 4 | 1200 | 800 | 50 |
| db.t3.large | 2 | 8 | 2400 | 1600 | 100 |
| db.t3.xlarge | 4 | 16 | 4800 | 3200 | 200 |

| GCP (`GCPDBCatalog`, us-central1 defaults) | VCPU | GiB | MaxIOPS | MaxConns | $/mo |
|---|---|---|---|---|---|
| db-f1-micro | 0.6 | 0.6 | 3000 | 250 | 11 |
| db-g1-small | 1 | 1.7 | 3000 | 250 | 43 |
| db-custom-1-3840 | 1 | 3.75 | 48000 | 1000 | 72 |
| db-custom-2-7680 | 2 | 7.5 | 48000 | 1000 | 144 |
| db-custom-4-15360 | 4 | 15 | 48000 | 1000 | 288 |
| db-custom-8-30720 | 8 | 30 | 48000 | 1000 | 575 |

(GCP MaxIOPS/MaxConns are documented baselines — Cloud SQL publishes no per-tier caps the way RDS does; they rarely discriminate between tiers, which reflects how the service behaves.)

## §10 Chart & risk aggregation — `dailySeries` / `dailyP95` / `windowP95`

Two similar-but-distinct reductions — don't confuse them:

- **Analysis** (`dailyP95`): per-day p95 of the day's 15-minute samples → then the p95/p99 **over the daily p95s** (used for request/limit, §2).
- **Series endpoint** (`dailySeries`): one point per UTC day carrying **p50/p95/p99/max of the day's window p95s** — `P50 = Percentile(vals, 50)`, `P95 = Percentile(vals, 95)`, `P99 = Percentile(vals, 99)`, `Max = max(vals)` — where `vals` = the window P95s (`b.P95`) in that day. The chart shows the day's p50/p95/p99/max bands.
- **Risk** (`windowP95`): p95 of each day's p95s, then p95 over the daily p95s — i.e. the analysis window statistic reused for near-headroom checks (§8).

## §11 Savings semantics — realized vs projected — `api/savings.go`

```
projected_monthly = Σ SavingsMonthly over pending recommendations
realized_monthly  = Σ SavingsMonthly over recommendations whose LATEST apply event Result == "applied"
                      and whose latest apply event has a passed verification verdict
                      (a later "reverted", failed verification, or inconclusive verification excludes it)
realized_yearly   = realized_monthly × 12
```

The latest-event + passed-verification rule is what makes the AC "realized from verified applies only" true: an apply that has not passed verification can never count, even if it is still applied. `by_owner` splits both numbers per `consize.savings.dev/owner` label (unassigned when absent).

**Worked example (live):** cluster projected $31.59/mo, realized $0.36/mo (the one verified M2 apply) — never conflated; two distinct tiles in the UI.

## §12 Confidence and the data-minimum gate

```
daysOfData  = count of distinct UTC days (WindowStart/86400) with ≥ 1 bucket
confidence  = min(daysOfData / ConfidenceDays, 1.0)      ConfidenceDays = 14
skip when   daysOfData < Config.MinDataDays              default 5; cluster bootstrap 0.1 (ADR-024)
```

**Worked example:** 7 days of data → confidence 0.5; 14+ days → 1.0. At the default minimum, a workload with 4 days is skipped ("insufficient data (4/5 days)").

## §13 The fixture demand model — `demandValue` (demo/tests only)

Retired from all live paths (ADR-035) but still shipped for tests and the zero-config demo:

```
h = (secs mod 86400) / 86400         # position in the day, 0..1
w = (secs mod 604800) / 604800       # position in the week, 0..1
value = base × (1 + 0.10 × sin(2πh) + 0.05 × sin(2πw))
```

Percents round to 1 decimal, counts to integers; each window stores P50=P95=P99=Max=value, Samples=1 (ADR-011). Fixture instance: `payments-prod` db.t3.large, base demand CPU 10%, mem 12.5%, IOPS 200, conns 300, errors 2/bucket; window `sun:00:00-sat:00:00`; `auto-db` label set. Golden outcome: `db.t3.medium`, $50/mo (§4).

## §14 Maintenance-window conventions

- Format: weekly UTC `ddd:hh:mm-ddd:hh:mm`; `end < start` wraps past midnight (`sun:23:00-mon:01:00`); `end == start` is malformed; empty or malformed → **fail-closed for every mode** (ADR-031 §1). Dry-runs are exempt from the *timing* check but report `InWindow`/`Window` and still fail closed on unconfigured/malformed.
- **GCP Admin API day convention: 1 = Monday … 7 = Sunday** (`gcpDays = ["mon","tue","wed","thu","fri","sat","sun"]` in `internal/dbmetrics/cloudmonitoring/cloudmonitoring.go`, verified live on consize-demo and test-locked; the shipped bug — mapping 1=Sunday — rendered a Sunday window as `sat:03:00` and was fixed, ADR-035 §2). Hour 23 wraps via `gcpDays[day%7]`; out-of-range → "" (fail-closed).
- Live instance: consize-demo window **sun:03:00-sun:04:00** (matches `gcloud sql instances describe`).

## §15 Pagination & retention

```
GET /api/v1/recommendations?limit=&offset=
  limit default 100, cap 500, reject limit < 1 / offset < 0 with 400
  response: { ..., "pagination": {"limit", "offset", "total"} }
  sort: savings descending, before slicing → pages compose into a stable global order
PruneRecommendations(status, cutoff): only superseded rows, only by age,
  CONSIZE_REC_RETENTION default 168 h; applied/verified/rolled_back/pending never pruned
```

---

# Part C — Terminology

| Term | Meaning |
|---|---|
| **poke** | The local development instance of the whole stack: API + Postgres (`127.0.0.1:18099`, DB on `54330`/`consize_test`) running against **real** data — real GCP Cloud Monitoring series for consize-demo and real cluster Prometheus (via `kubectl port-forward`, `CONSIZE_WINDOW=3h` locally). The Next.js UI runs against it at `:3000`. Origin of the name: "poke at it with a stick" — the always-on scratch environment to probe behavior. |
| **headroom** | The safety margin between projected usage and the capacity being provisioned. Two distinct uses: (1) **request headroom**: request = p95 × 1.2, so the p95 sits at ~83% of the request; (2) **DB headroom caps**: candidate classes must keep projected utilization *below* 60% CPU / 60% IOPS / 75% memory / 70% connections (ADR-030 §9). "Near headroom" = within 10 points of a cap (risk flag). |
| **p50 / p95 / p99** | Percentiles of the 15-minute window series, aggregated per UTC day, then over the daily series (§1, §2). p95 sizes requests, p99 sizes limits (via max(p99, 2×request)); p50/p95/p99/max are the chart bands. **Never averages** (ADR-002). |
| **percentile (linear interpolation)** | The engine's percentile definition: `v[floor(r)] + frac(r)·(v[floor(r)+1] − v[floor(r)])`, `r = p/100·(n−1)` (§1). |
| **window / bucket** | One 15-minute sample of one metric for one workload, stored in `usage_buckets` upserted on `(workload, metric, window_start)` (ADR-007). A day = 96 windows. Single-sample in v1 (P50=P95=P99=Max, ADR-011). |
| **daily p95 / window p95** | Two aggregations, don't confuse: `dailyP95` = p95 of a day's 15-min samples (analysis input); `windowP95` = p95 of the daily p95s, then p95 over them (risk's near-headroom statistic) (§10). |
| **millicores (m)** | CPU units as k8s expresses them: 1000 m = 1 vCPU. Savings math divides by 1000 to get cores (§3). |
| **GiB / MiB** | Binary units (1 GiB = 2³⁰ bytes): k8s memory and the pricing tables use them; `MemPerGiBMonth` is $/GiB-month. |
| **step / follow-up** | An apply never jumps the whole reduction: k8s applies move ≤ 30% of the current value per apply; DB applies move exactly one adjacent class. The remainder is queued as a **follow-up** pending recommendation, blocked until the current step verifies — the apply → verify → apply rhythm (ADR-020, ADR-031 §2, §5). |
| **guardrail** | A rule that can block an apply before it touches anything: store health (readyz), pending-only, exclusions, auto-apply label policy, concurrency (1 in-flight), step ≤ 30% / one class step, maintenance window, approval mode. Blocks return structured 422 `{"error":"apply blocked","reasons":[...]}`. |
| **maintenance window** | The weekly UTC time range an instance may be changed: `ddd:hh:mm-ddd:hh:mm`, wraps at midnight, fail-closed when empty/malformed (ADR-031 §1, §14). |
| **no-evidence** | A metric a provider cannot produce (`db_errors` on CloudWatch; `db_iops`/`db_errors` on Cloud Monitoring). No-evidence **never FAILs** verification — it is not evidence of health (ADR-034 §2, ADR-035 §2). |
| **fixture** | The deterministic demo DB source (`dbmetrics.Fixture`: payments-prod, golden demand, sinusoid model §13). **Retired from all live paths** as of ADR-035; kept for tests and the zero-config demo. |
| **projected vs realized savings** | Projected = Σ pending recommendations; realized = Σ recommendations whose *latest* apply event is still `applied` and whose latest apply has a `passed` verification verdict (reverted/failed/inconclusive excludes; ×12 for yearly) — never conflated, two distinct UI tiles (§11). |
| **confidence** | 0..1 = min(daysOfData/14, 1) — how much of the ideal 14-day window a recommendation is based on; scales with data volume, never inflated by lowering `CONSIZE_MIN_DATA_DAYS` (§12). |
| **risk / risk pill** | low | medium | high computed from existing data (data days, saturation near caps, step distance, window state, follow-up state, data-loss-risk label); reasons in `risk_reasons`, rendered as a pill + tooltip (§8). |
| **superseded** | A recommendation replaced by a newer analysis cycle for the same workload/resource; the only rows ever pruned, and only by age (ADR-029 §2). |
| **dry-run** | Apply mode that records a `planned` event and touches nothing; exempt from the maintenance-window *timing* guard but still reports `InWindow`/`Window` and fails closed on unconfigured/malformed windows; never queues follow-ups (ADR-031 §1,§3). |
| **verifier / SLI / verdict** | The `cmd/verify` batch binary compares baseline vs post signals (SLIs) and records a three-valued verdict: **passed** | **failed** (→ auto-rollback + alert) | **inconclusive** (terminal, never rolls back; blocks further applies until a human looks) (ADR-006/018/022/027, §6–§7). |
| **rollback** | On FAIL: k8s — restore **pre-apply values absolutely** via a live `live → pre-apply` diff (drift-proof, ADR-026); DB — restore the absolute pre-apply class recorded in the apply event (ADR-032 §6). Recorded as a `reverted` apply event. |
| **RBAC / least privilege** | The collector uses `consize-reader`: read-only ClusterRoleBinding for cluster-wide discovery when `CONSIZE_NAMESPACES` is empty, or namespace-scoped reads when set. Direct apply/rollback use the separate `consize-writer` identity, bound only in explicitly write-enabled namespaces; `mode=auto` additionally requires the auto-apply label. IaC PR mode needs GitHub access, not Kubernetes write RBAC. |
| **CDP smoke** | Headless-Chrome (Chrome DevTools Protocol on `127.0.0.1:9222`) end-to-end UI check: load pages, poll rendered text for hit markers, count `window.__rejected` promise rejections. Used to verify the Next.js UI (and before it, the embedded SPA) against the poke. |
| **API_UPSTREAM / rewrite** | `next.config.ts` rewrites `/api/v1/:path*` → `API_UPSTREAM` (default `http://127.0.0.1:18099`): the same Next.js build works against poke, cluster, or cloud API; `NEXT_PUBLIC_API_BASE` is the escape hatch; no CORS (ADR-036 §2). |
| **auto-apply label** | `consize.savings.dev/auto-apply=enabled` (namespace) / `consize.savings.dev/auto-db=enabled` (DB instance) — the only way automatic application is allowed; everything else needs an explicit approved actor (ADR-004, ADR-031 §3). |
| **usage_buckets** | The store table holding every workload's metric windows; the single data source for analysis, risk, series charts, and DB verification. |
| **backfill** | Re-running collection for a past range; safe because upserts are idempotent on `(workload, metric, window_start)` (ADR-007). |
| **source / provider** | The `dbmetrics.Source` seam (`ListInstances`, `Series`): `fixture` (tests/demo), `cloudwatch` (RDS, ADR-034), `cloudmonitoring` (GCP, ADR-035); selected by `CONSIZE_DBMETRICS`. The verifier never uses it — DB verification reads the store (ADR-030 §7). |
| **surface** | k8s compute vs cloud databases — the two workload surfaces; the API's series endpoint is surface-aware (units millicores/bytes vs percent/iops/connections/errors, ADR-034 §3) and the UI has one unified filter across both. |
| **class / instance class** | A provider DB tier (e.g. `db.t3.medium`, `db-custom-1-3840`) with catalog VCPU/GiB/IOPS/conns capacity and price; provider-scoped catalogs, never merged (ADR-035 §3). |
| **step plan** | The dry-run's report of the one adjacent class step (DB) or ≤30% step (k8s): `Current`, `Proposed`, `TotalSteps`, `InWindow`, `Window` — what the apply *would* do. |
| **sustained minutes** | How long a signal must stay past its threshold to count as a breach (default 5; `CONSIZE_SUSTAINED_MINUTES`). The live E2E used 3 for a shorter demo window. |
| **realized** | See projected vs realized. The M4 AC "realized from verified applies only" is the latest-event + passed-verification rule in §11. |
| **session** | A revocable login: 32-byte random token issued at login, stored in Postgres as a SHA-256 hash only, 7-day TTL, deleted on logout or on expiry; carried in the `consize_session` httpOnly + SameSite=Lax cookie (+ Secure behind TLS) (ADR-037 §2). |
| **role** | `viewer` (read-only; apply calls rejected server-side with 403) · `operator` (approve + apply) · `admin` (policy/settings) — stored in `users.role`, enforced by the API, never by the UI alone (ADR-037 §3). |
| **actor** | The identity recorded on every apply event: `operator` (legacy self-reported), `auto` (label-automated), or `api:<email>` — the session-verified actor since ADR-037; on auth-enforced deployments clients can no longer self-report identity. |
| **bootstrap admin** | The first user, created by `CONSIZE_BOOTSTRAP_ADMIN="email:password"` while the users table is empty — the only out-of-band credential (ADR-037 §6). The first-run wizard (`POST /auth/setup`, `needs_setup` flag) is its interactive equivalent for ad-hoc deployments — same one-admin-ever gate. |
| **auth_enabled** | The `GET /auth/me` flag distinguishing "login required" (true) from "auth not enforced" (false, demo build); the UI shows the login page only for the former (ADR-037 §6). |
| **bootstrap minimum** | `CONSIZE_MIN_DATA_DAYS=0.1` on the live analyze CronJob — a documented temporary value so the fresh consize-demo instance is analyzable at all; restore the default 5 once it holds 5 days of data (ADR-024, ADR-035 §4). |

---

*Maintainers: when a decision changes, update [decisions.md](decisions.md) first (canonical), then this file, then [features.md](features.md) and [plan.md](plan.md).*
