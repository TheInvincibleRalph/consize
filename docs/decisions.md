# Consize — Decision Log (ADRs)

## ADR-001: Go for the engine, React for the UI

**Status:** Accepted · **Date:** 2026-08-23

**Context:** One binary family (collector, analysis, apply, verifier, API) touching k8s and cloud SDKs; a dashboard for charts and audit views.

**Decision:** Engine in Go (single module, vertical-slice packages, `cmd/` per binary). UI in React + Vite + Tailwind, served as static assets behind the API.

**Consequences:** Great k8s/cloud SDK ecosystem (`client-go`, AWS SDK), easy static binary + distroless images, single language for the team of one. Trading away TypeScript end-to-end — acceptable: the API contract (OpenAPI) is the shared type boundary.

## ADR-002: Percentile-based sizing over averages

**Status:** Accepted

**Context:** Average usage hides bursts; max usage recommends the wrong size.

**Decision:** Recommendations derive from p95 (request sizing) and p99 (limit sizing) over 14 days of 15-min buckets, with explicit headroom multipliers. No averages, ever.

**Consequences:** Robust to spikes and weekends; produces slightly larger requests than an average-based approach — correct tradeoff: Consize's job is *safe* savings, not maximal savings.

## ADR-003: Downsize-only in v1

**Status:** Accepted

**Context:** Upsizing recommendations (e.g., a workload is throttling) are valuable but expand the safety surface significantly.

**Decision:** v1 recommends only reductions; upsizing is an explicit future toggle. Throttling/OOM detection still reports the evidence — visibility without action.

**Consequences:** Smaller blast radius, clearer demo story. The "find throttling" signal is preserved for the roadmap.

## ADR-004: Auto-apply is opt-in per namespace

**Status:** Accepted

**Context:** Auto-apply is the differentiator ("platform, not report") but unconditional auto-apply is how trust dies in one incident.

**Decision:** Auto-apply runs only in namespaces labeled `consize.savings.dev/auto-apply=enabled`; everything else requires explicit approval. Exclusions always win. Step limit ≤ 30% per apply.

**Consequences:** Safe default, trust-by-config. The demo shows both paths (approved apply + auto-apply namespace).

## ADR-005: Postgres for the store

**Status:** Accepted

**Context:** Needs: audit trail with INSERT-only tables, range queries over time buckets, FK integrity, simple operations for one person.

**Decision:** Managed Postgres (RDS/Cloud SQL) outside the managed cluster — Consize never right-sizes its own database.

**Consequences:** Battle-tested, familiar, trivially backed up. Trade-off: not columnar like ClickHouse — fine at v1 scale (10k workloads × buckets/day); revisit if we ingest at fleet scale.

## ADR-006: Verification via SLI comparison, not chaos

**Status:** Accepted

**Context:** Rollback decision needs a signal that is *measured from the system*, cheap, and defensible.

**Decision:** Baseline 24 h pre-apply vs 24 h post-apply on error rate, p99, throttling, OOM/evictions (k8s) and CPU saturation/connections (DB); threshold breaches sustained 5 min → FAIL → automatic rollback.

**Consequences:** No traffic injection or synthetic probing needed in v1; rollback evidence is charts a skeptic accepts. Weakness (accepted): quiet systems may yield INCONCLUSIVE — handled by explicit verdict, never silence.

## ADR-007: Idempotent bucketed collection, backfill-first

**Status:** Accepted

**Context:** Collectors crash, windows overlap, Prometheus retention varies.

**Decision:** Upsert by `(workload, metric, window_start)`; bucketing keyed on source timestamp; explicit backfill flag replays history.

**Consequences:** Safe re-runs, no dupes, easy recovery. Confidence scoring uses data volume so sparse windows degrade recommendations, not crash them.

## ADR-008: Apply never runs without an audit path

**Status:** Accepted

**Context:** The store (audit trail) is the source of truth for "what did Consize do and why."

**Decision:** Any apply requires a prior `apply_event` row; if the store is unhealthy (`readyz` fails), applies are blocked regardless of cluster health.

**Consequences:** Fail-safe by construction; audit is a hard dependency, not a feature.

## ADR-009: One repo, one version, one release

**Status:** Accepted

**Context:** Solo build; versions across engine/UI/infra/charts must move together.

**Decision:** Monorepo, semver from commit 1, single release workflow (tag → CI → scan → sign → chart + image).

**Consequences:** Simple mental model, atomic releases. Trade-off: infra and engine version-locked — accepted at this scale; revisit at fleet mode.

## ADR-010: Self-hosted on the cluster it manages

**Status:** Accepted

**Context:** The product's own story is "run Consize like you'd run Consize-managed workloads."

**Decision:** Consize deploys into the cluster it analyzes (separate namespace, network-isolated), with its own Postgres managed separately.

**Consequences:** The demo is dogfooding: Consize's own manifests are rightsized by Consize (after an exclusion for safety in early milestones). Honest production story, real maintenance pressure — that's the point.

---

## M1 decisions (recorded 2026-08-24)

## ADR-011: Single-sample 15-minute windows in v1

**Status:** Accepted

**Context:** The engine's day = 96 buckets math assumes one value per 15-minute window. Sampling inside the window (e.g. 1-minute resolution aggregated into p50/p95/p99/max per window) costs 15× the Prometheus load for percentile precision that daily-p95 aggregation barely uses.

**Decision:** The collector queries Prometheus once per metric at 15-minute steps. Every window stores P50=P95=P99=Max = the sampled point; sub-window sampling is a future refinement that can land without schema or policy changes (the columns exist).

**Consequences:** One query per metric per cycle; the policy (per-day p95 → window percentiles) still smooths intra-day variation. Consequence of the simplification: a single bad scrape skews one window — bounded because a window is 1/96 of a day's p95.

## ADR-012: Store behind an interface, memory impl for tests

**Status:** Accepted

**Context:** Every component (collector, analyze, API) needs persistence; tests must not require a database; Postgres and in-memory must never drift semantically.

**Decision:** All engine code depends on the `store.Store` interface only. `Memory` and `Postgres` implement identical semantics (idempotent upserts, supersede-on-create, denormalized reads). The behavior suite runs against both; Postgres integration is gated by `CONSIZE_TEST_POSTGRES` and skipped by default.

**Consequences:** Tests are fast and hermetic; a schema bug can hide in Postgres-only paths until CI runs the gated suite (CI runs it against an ephemeral Postgres). `store.Open` falls back to memory when `DATABASE_URL` is unset so every binary demos zero-config.

## ADR-013: Embedded versioned migrations, run at startup

**Status:** Accepted

**Context:** Schema must evolve with the code, from a single source, without a separate toolchain.

**Decision:** SQL files are embedded (`//go:embed`), applied in filename order inside a transaction, tracked in `schema_migrations` (idempotent). `store.Open` migrates on startup; `cmd/migrate` exists for initContainers/CI.

**Consequences:** Fresh and existing deployments both work; migration correctness is testable via the gated Postgres suite. Trade-off: no down-migrations — the audit-trail principle (see ADR-008) makes destructive down-migrations undesirable anyway.

## ADR-014: Pricing degrades, never fails

**Status:** Accepted

**Context:** Savings math needs a rate table; the AWS Price List API can be unreachable, slow, or rate-limited — and analysis must still run.

**Decision:** Three layers: `Static` (shipped GKE-style defaults), `AWS` (SigV4 fetch of the EC2 on-demand index, median $/vcpu-hr and $/GiB-hr × 730h, TTL-cached 24 h), and `Resilient` (falls back to static on any primary error, with a warning log). The API exposes the active price table alongside savings.

**Consequences:** Recommendations and savings always compute; a fallen-back table is visible in the payload rather than silently wrong. Trade-off: median-across-instances rates are approximations of a specific fleet — acceptable for projections, and the table is data the API can later refine per instance family.

## ADR-015: Bulk pod→deployment resolution, not per-pod lookups

**Status:** Accepted

**Context:** Mapping Prometheus pod series to workloads requires pod→deployment ownership. Per-pod GETs are N+1 queries; ownerReferences only point pod→ReplicaSet.

**Decision:** Three bulk list calls per cycle: ReplicaSets (map RS→Deployment), Pods (map pod→RS), and Deployments (metadata). Series whose owner doesn't resolve to a listed deployment are dropped with a log line — never attributed to a wrong workload.

**Consequences:** O(3) API calls regardless of fleet size; unknown series degrade to "not managed" silently. Trade-off: ownership changes mid-window are resolved at collection time, matching the deployment metadata snapshot — consistent because both come from the same cycle.

## ADR-016: Half-known windows are dropped in bucket merge

**Status:** Accepted

**Context:** CPU and memory arrive as separate bucket rows. A window present in only one series (one query failed, retention gap) would supply a zero for the other metric — dragging daily p95s toward zero and fabricating "already optimal" recommendations.

**Decision:** `cmd/analyze` merges the two series on `window_start` and drops windows missing either metric. Both queries run at identical steps, so real windows virtually always match.

**Consequences:** Input hygiene is conservative by construction: a recommendation is never made on half-truths. Cost: a partial window's data is wasted — negligible, and re-collection (ADR-007) restores it.
## ADR-017: Limits ride with recommendations; downsize both together

**Status:** Accepted

**Context:** M1 recommendations sized requests only. A CPU request cut without a matching limit cut does nothing (the limit still gates the pod); memory limits unedited keep the OOM risk unchanged. The apply engine needs the full pair to make a safe, meaningful change.

**Decision:** The analysis engine carries `current_limit`/`proposed_limit` alongside the request values on every recommendation (equal values = unchanged). The patcher always writes both, per the same step policy. Request-only granularity was considered and rejected: it halves the savings claim while leaving risk-bearing limits untouched.

**Consequences:** Sizing math grows one dimension (limit = max(2×request, p99) as before, just carried through). Follow-up steps (ADR-020) carry both values, and the verifier's rollback restores both.

## ADR-018: The verifier is a one-shot CronJob binary, not a service

**Status:** Accepted; fixed 24 h window superseded by ADR-048

**Context:** Verification needs a full 24 h post-apply window, then runs for a few seconds per event. A long-lived daemon would idle between verdicts, hold credentials continuously, and need its own liveness story.

**Decision:** `cmd/verify` is a batch binary: `AppliedEventsUnverified()` → for each event whose window is due → compare SLIs → record the verdict → roll back + alert on FAIL → exit. Runs on a CronJob every minute, so due applies are processed automatically as soon as their safety window opens. Each tick costs almost nothing when idle. Concurrency is safe by construction: in-flight applies are excluded from new applies by the store's derived state, and `CreateVerificationRun` upserts per apply event so a retried tick overwrites rather than duplicates.

**Consequences:** Verdict latency is bounded by the schedule plus the window.
ADR-048 later replaced the fixed 24 h window with a 1 h step-scaled base
window. If real-time verification is ever wanted, the same `verifier.Service`
slots into a daemon unchanged.

## ADR-019: Workload-scoped kubelet-native SLIs, app-level metrics opt-in

**Status:** Accepted

**Context:** Per-service SLIs need application instrumentation, which most clusters don't have. The verifier must still tell a good downsize from a regression using signals that exist in any standard cluster.

**Decision:** v1 verifies four kubelet/cadvisor/kube-state-metrics signals scoped to the Deployment that changed — throttling (`container_cpu_cfs_throttled_seconds_total`), OOM kills, restarts, evictions — with 1-minute resolution so "sustained ≥ 5 min" is measurable. Deployment scoping uses the workload's namespace plus its generated pod-name shape (`<deployment>-<pod-template-hash>-<pod-id>`) so a noisy sibling workload cannot trigger a rollback for a healthy apply. Error-rate and p99 expressions are configurable via `CONSIZE_SLI_ERROR_EXPR`/`CONSIZE_SLI_P99_EXPR` (rate-wrapped, `sum by (namespace)`), default off until teams provide app-level labels.

**Consequences:** Zero-instrumentation verification covers the whole fleet without blaming one workload for another workload's restarts or OOMs. A workload with no SLI data at all is *inconclusive*, never a pass (ADR-022). App-level SLIs ride in when teams provide expressions; the evidence schema doesn't change.

## ADR-020: Step splits materialize as follow-up pending recommendations

**Status:** Accepted

**Context:** A 70% reduction needs several 30% applies. The remainder must be *actionable*, not just logged, or the savings never land. But inserting it must not trigger the "new analysis supersedes old" rule — the follow-up *is* the step plan, not fresh analysis output.

**Decision:** After a real apply, the remainder is inserted as a pending follow-up recommendation via `CreateFollowUpRecommendation` — which does NOT supersede existing pending recommendations. Its savings scale by the request-reduction share of the remaining step. The follow-up cannot apply until the current step verifies: the in-flight guardrail blocks it, producing the apply → verify → apply rhythm by construction.

**Consequences:** Overlap with fresh nightly analysis output is possible (both may hold pending recommendations for the same workload); a nightly analyze run clears it when the step finishes. Recoverable and visible in the API, never silent.

## ADR-021: Proportional patch distribution with exact-sum rounding and QoS preservation

**Status:** Accepted

**Context:** A deployment's containers share one request/limit budget. Distributing a delta naively (equal shares, or per-container percentiles) over- or undershoots and can break QoS classes.

**Decision:** `K8sPatcher` distributes each resource's step across containers proportionally to each container's current request share; the last container absorbs rounding so the containers' totals land *exactly* on the proposed values. QoS rule: a container keeps exactly the request/limit fields it already declared — never add a field it lacks (that would change its QoS class), never remove one; containers with neither field are untouched. Writes go through GET → mutate → `Update` guarded by resourceVersion, with up to 3 conflict retries on a fresh read.

**Consequences:** Multi-container workloads stay internally consistent and QoS-stable. The write path is one surface shared by apply and rollback (the verifier never touches the cluster itself).

## ADR-022: Inconclusive never rolls back; nothing-measurable is inconclusive

**Status:** Accepted

**Context:** A FAIL verdict triggers an automatic, cluster-touching rollback — the highest-stakes decision in the system. The inverse (missing data) must not be able to trigger it, and must not be papered over as a pass.

**Decision:** Verdicts are three-valued: `passed` | `failed` | `inconclusive`. Rollback fires only on `failed`. A signal with data in one window but not the other is inconclusive; a verification where *no* signal could be judged at all is also inconclusive — "never silence" (ADR-006). Inconclusive runs are recorded as rows so they're visible, and the CronJob re-tries them on later ticks only when the window was premature — a genuinely unverifiable event stays inconclusive forever, blocking further applies in its namespace until a human looks (safe default: no silent regressions, no silent savings).

**Consequences:** Quiet clusters get flagged, not auto-approved. The trade-off is operational: an inconclusive namespace needs human attention before the next apply — which is exactly the safety posture v1 wants.

## ADR-023: Append-only apply trail; in-flight state is derived

**Status:** Accepted

**Context:** The audit trail (ADR-008) must be immutable evidence — but "is an apply in flight?" is a state question that changes as verification completes. Storing state alongside events would invite UPDATEs and lies.

**Decision:** `apply_events` is INSERT-only (results are new rows: planned → applied → reverted). In-flight state is *derived*: an `applied` event with no row in `verification_runs`. A crash between patch and verification row leaves a retryable state, not a corrupted record; the verifier's upsert (`ON CONFLICT (apply_event_id)`) keeps one row per event while allowing a re-run to overwrite a premature inconclusive.

**Consequences:** Every claim about the trail is provable from the row sequence; the fail-safe (ADR-008) keeps the store's health as a hard gate on applies. The store's DB role is INSERT-only on these tables per security.md.

## ADR-024: The data-minimum is a configurable confidence gate (CONSIZE_MIN_DATA_DAYS)

**Status:** Accepted

**Context:** Analysis skips workloads with fewer than 5 days of data (`MinDataDays`) — a statistical-confidence rule. But it is not a *safety* rule: the verifier independently protects every apply, whatever data it was based on. A hard-coded 5 blocked legitimate use cases (new fleets, ephemeral environments) and made the live-cluster E2E impossible — the test cluster's Prometheus is ephemeral (no PVC) and no workload carries 5 days of history.

**Decision:** `MinDataDays` becomes `analysis.Config.MinDataDays` (float, "distinct days with data"), shipped default 5; `Analyze` keeps its signature and delegates to `AnalyzeCfg` with the default so existing behavior is byte-identical. `cmd/analyze` reads `CONSIZE_MIN_DATA_DAYS` (default 5) via a new `config.Float` helper. The confidence score still scales with data volume, so a lowered minimum never inflates confidence.

**Consequences:** Operators can trade statistical confidence for cycle speed on new fleets; the shipped default is unchanged. The live E2E uses 0.1 ("any data within the window") because no workload has meaningful history on this cluster — the verifier remains the safety gate.

## ADR-025: Collection scoping and read/write RBAC split (CONSIZE_NAMESPACES)

**Status:** Accepted

**Context:** The collector lists deployments, replica sets, and pods — historically cluster-wide. Early E2E runs proved the namespace-scoped read model by binding `consize-reader` only in analyzed namespaces; a cluster-scope list correctly failed under that Role. The production architecture now needs a broader enterprise mode: discover the whole cluster without broadening the write surface.

**Decision:** `CONSIZE_NAMESPACES` ("ns1,ns2") scopes workload listing to the named namespaces. Empty keeps cluster-wide discovery and is paired in production with a read-only `consize-reader` ClusterRoleBinding. The write identity remains separate: `consize-writer` is bound only by namespaced `consize-apply` RoleBindings where Direct apply is explicitly allowed. `mode=auto` additionally requires `consize.savings.dev/auto-apply=enabled`. IaC PR mode does not require Kubernetes write RBAC. Implemented as `NewK8sMetadata(kubeconfig, namespaces)` with `listNS()` returning the configured set or `NamespaceAll`; `ListDeployments` and `PodOwners` loop the resolved set.

**Consequences:** Operators can choose namespace-scoped collection for a team install or cluster-wide discovery for an enterprise install without granting cluster-wide write. Direct runtime changes stay namespace opt-in; source-of-truth changes go through the GitHub/IaC PR workflow. A RoleBinding's RoleRef resolves in the binding's namespace only, so the write Role must exist in every namespace that explicitly enables Direct apply.

## ADR-026: Rollback restores pre-apply values absolutely (drift-proof)

**Status:** Accepted

**Context:** A FAIL verdict rolls back by re-patching the workload. The original implementation inverted the apply diff (`Current ↔ Proposed` swap) and handed it to the patcher, which distributes `Proposed − Current` over the containers' *live* requests. That is correct only while the live state still equals the state the apply left behind. The live-cluster E2E proved the assumption wrong: the verifier catches exactly the case where an external actor (a bad release) drifts the workload *during* the window — a swapped-diff rollback then landed on `live + (preApply − applied)` (64 MiB + 23 MiB = 87 MiB), restoring nothing and leaving the workload still below its allocator footprint.

**Decision:** `Rollback` now reads the live totals (`Patcher.ReadResources`) and patches with an honest `live → pre-apply` diff. The patcher's proportional distribution then lands the totals *exactly* on the recorded pre-apply values regardless of drift; the recorded `reverted` event carries that diff, so the audit trail shows the actual restore. Pre-apply restore — not re-applying the recommendation's values — because a FAIL means the change was harmful, and "undo our change" is the only semantic that is safe both when the recommendation itself caused the regression and when an external actor did.

**Consequences:** Rollback is drift-proof by construction, unit-tested with a stateful fake patcher (`TestRollbackAfterDrift` mirrors the live failure: apply → external drift → rollback lands on pre-apply). The patcher interface gains one read method; no store/API changes — the `reverted` event still records the inverse intent.

## ADR-027: Inconclusive is terminal, and that is the safe failure mode (live E2E data loss)

**Status:** Accepted

**Context:** During the live E2E, the ephemeral monitoring Prometheus (no PVC) was recreated mid-run; all pre-16:40Z history vanished. A canary apply whose verification was already due then evaluated with an empty baseline window: every SLI returned "data missing in one window", the verdict was recorded `inconclusive`, and — correctly — no rollback fired. The event is unverifiable forever (baseline data cannot be reconstructed), so the run could not be salvaged; it had to be re-run on a fresh apply with intact baseline data.

**Decision:** `inconclusive` stays terminal for the event it names, and never rolls back: without baseline evidence, Consize cannot distinguish "the change harmed the workload" from "the data disappeared". Rolling back on absent data would let an infrastructure failure (or a compromised metrics path) veto a good change. The verdict's purpose is to force *human* attention, not automated action. If an operator restores the baseline (or the metrics path returns), the verifier's upsert (`ON CONFLICT (apply_event_id)`, ADR-023) lets a manual re-run overwrite the inconclusive row.

**Consequences:** The audit trail keeps honest "unknown" rows instead of fabricating failures; the E2E re-ran the FAIL scenario on a fresh apply rather than force-fitting the lost event. Operationally, teams that run Consize with ephemeral Prometheus storage must expect occasional inconclusive verdicts and treat them as "re-run after data returns" — the shipped default window (24 h, ADR-018) makes data loss inside a window the exception, not the rule. The plan's pre-existing caveat ("no PVC → history resets") was updated in docs/e2e.md to say this actually happened, not just that it could.

## ADR-028: Verification requires durable SLI history (live E2E, data-loss round two)

**Status:** Accepted

**Context:** The live E2E's monitoring Prometheus (kube-prometheus-stack, no PVC) lost its entire history twice during the run. First ~16:40Z (pod recreated; pre-16:40Z data gone → the due verification returned inconclusive, ADR-027). Second ~21:26Z — this time root-caused: the cluster's `application-pool` has node autoscaling enabled, and scale-downs replace nodes; the monitoring Prometheus (a StatefulSet with an emptyDir) reschedules to a fresh node and its WAL dies with the old one. Three nodes were created in a single hour (20:48Z, 21:12Z, 21:26Z); each replacement wiped every series the pod had scraped. The canary's run-4 verify window (apply 20:39Z, wipe 21:26Z) lost baseline *and* post data — the event became unverifiable and had to be superseded a second time.

**Decision:** Durable SLI storage is a prerequisite for verification, not an optimization. The monitoring Prometheus received a 10 Gi `standard-rwo` PVC via its Prometheus CR (`spec.storage.volumeClaimTemplate`), so pod/node churn no longer erases history. Consize itself cannot repair this — the verifier is only as sound as the metrics path it reads, and `inconclusive` (ADR-027) is the honest symptom of a broken one. The verify window (shipped default 24 h, ADR-018) is effectively an SLA on the metrics path: fleets that run Consize against ephemeral Prometheus storage must expect terminal inconclusive events whenever node/pod churn lands inside a window.

**Consequences:** The E2E's fresh canary cycle runs on durable storage; the environment caveat in docs/e2e.md now reads "fixed with a PVC" instead of "history resets". Operational guidance for Consize operators: persist and size the SLI store so it outlives node churn, or treat inconclusive verdicts as a scheduled maintenance artifact rather than a signal.

## ADR-029: Recommendations pagination, superseded pruning, and an embedded read-only dashboard (M2 debt closure)

**Status:** Accepted

**Context:** Two debts accumulated as M2 landed. (1) `GET /api/v1/recommendations` returned every matching row unbounded — on the live cluster that is 400+ rows (~69 KB unfiltered), too much for a page and for the collector/analyze cycles that keep adding superseded rows. (2) The product had no surface to show anything: the first read-only dashboard was built by a separate frontend agent (`engine/ui/`, vanilla JS + hand-rolled SVG, no external assets) and needed a home. The live recommendations table at handover time held 418 rows — pending rows are the plan's work queue, but each analyze cycle supersedes the previous pending batch and the superseded rows are pure history with no read path and no bound.

**Decision:** Three coupled decisions, shipped as one work package:

1. **Pagination is server-side and total-aware.** `ListRecommendations` gains `limit, offset` (limit ≤ 0 = no limit; offset slices before limit) and returns the matching `total` before slicing. The handler defaults `limit=100`, caps at 500, rejects `limit < 1` / `offset < 0` with 400 (never silently defaulted), and every response carries `"pagination": {"limit", "offset", "total"}` so clients can render "N of M" and know when to stop fetching. Sorting (savings descending) happens before slicing, so pages compose into a stable global order.
2. **Only superseded rows are ever pruned, and only by age.** `PruneRecommendations(status, cutoff)` runs at the end of every analyze cycle with `CONSIZE_REC_RETENTION` (default 168 h). Applied/verified/rolled_back/pending are never pruned: the audit of what was *applied* lives in `apply_events` and `verification_runs` (never pruned, ADR-023), and pending rows are the live plan. Superseded rows are replaceable by construction, so pruning them loses nothing but bytes.
3. **The dashboard is embedded in the API binary** (`package ui`, `engine/ui/` — moved from repo root because `go:embed` cannot cross the module root). One Deployment serves the JSON API and the page on the same origin: no CORS, and the UI versions atomically with the backend it consumes. The app is hash-routed; the only server paths are `/` and asset requests, so any other non-API path falls back to `index.html`. Unknown `/api/*` paths stay honest 404s — the SPA fallback never masks a wrong endpoint.

**Consequences:** The live cluster now runs `api:e2e-v2` / `analyze:e2e-v2` (amd64, rebuilt cross-arch after docker.io proved unreachable from the dev machine) and serves the dashboard at `/` with pagination live (`pagination.total` = 418 at rollout). Known v1 wart, deliberately not fixed now: wire field names are PascalCase Go struct fields (no json tags) — the UI was built against that; introducing snake_case tags is a contract change that belongs with the M4 API freeze, not the debt pass. Charts were skipped because no usage-buckets endpoint exists yet — that lands with M3's data surface.

## ADR-030: M3 data surface — databases unify into the existing model

**Status:** Accepted

**Context:** M3 adds a second surface — cloud databases (RDS/Cloud SQL) — to an engine built around one shape: workloads → buckets → recommendations → apply events → verification runs. The acceptance criteria demand "one dashboard, one savings number" (AC A4), so a parallel DB surface (separate tables, separate recommendations, separate savings) would fail the milestone by construction. The `workloads` table already anticipated this: its `source` column's comment reads "k8s | db (the M3 surface)". The design question was how much machinery DBs can ride without lying about their differences.

**Decision:** Databases are first-class members of the existing model, distinguished by source, not by new tables:

1. **DB instances are Workloads** (`Source="db"`, `Kind="database"`, `Namespace` = provider namespace like "rds" or the k8s namespace of the pod). `Workload` gains four DB-only fields, empty for k8s workloads: `DBClass` (e.g. `db.t3.medium`), `DBReplicas`, `DBMaintenanceWindow` (UTC `ddd:hh:mm-ddd:hh:mm`), `DBProvider` (`aws` | `gcp` | `fixture`).
2. **DB metrics ride usage_buckets** with new metric names: `db_cpu_percent`, `db_iops` (absolute count), `db_connections` (absolute), `db_mem_percent`, `db_errors` (counter for the verifier). IOPS and connections are stored absolute, not as percentages: the denominator is the candidate class's catalog baseline, so percent must be computed per-class, not baked into the store.
3. **DB recommendations are Recommendations with `Resource="class"`** plus `ClassCurrent`/`ClassProposed` (empty for cpu/memory recs). Savings, status lifecycle, supersede-on-reanalysis, and `SavingsSummary` all work unchanged — one savings number falls out for free (AC A4).
4. **Class diffs ride `Diff`** (new `current_class`/`proposed_class` JSON fields); `apply_events.diff` is JSONB, so no schema change beyond the Go struct.
5. **Class catalog + analysis live in `internal/analysis`** (`db.go`), pure and golden-tested like the k8s policy. The shipped catalog is RDS-style classes with documented default rates (mirroring `DefaultPrices`); overridable via config.
6. **DB apply is its own service (`internal/dbapply`)** with the same guardrail philosophy as the k8s apply engine: store health, pending-only, exclusions win, mode policy, concurrency, audit-first. It adds two DB-specific guardrails — **maintenance-window enforcement** (the current UTC time must fall inside the instance's window; `now` is injected for tests) and **one-class-step** (the proposal must be an adjacent class; larger moves become follow-up recommendations, the same stepping philosophy as the k8s ≤30% rule). **Approval is the default**: `mode=auto` requires the instance label `consize.savings.dev/auto-db=enabled` (the plan's "auto-db opt-in flag"); everything else needs an explicit actor.
7. **DB verification reads the store, not a live provider.** DB metrics are ingested by the collector (CloudWatch/Cloud Monitoring → usage_buckets), so the verifier's DB path reads buckets from the store — one durable source of truth, same verdict semantics (passed/failed/inconclusive, ADR-006/027), with DB SLIs: CPU saturation, connections, error counters. Rollback restores the previous instance class via the same admin interface.
8. **Provider access is a seam, not a dependency.** `internal/dbmetrics` defines the collector-side `Source` interface (`ListInstances`, `Series`) with a deterministic fixture implementation for tests and the live-cluster demo. Live CloudWatch (RDS) and Cloud Monitoring (Cloud SQL) collectors are deferred — the exact precedent of M1, which deferred GCP pricing behind `pricing.Service`. The verifier needs no provider interface at all (decision 7).
9. **The plan's headroom thresholds are resolved by interpretation** (the bullet "CPU < 60%, IOPS > 40%, mem > 25%, conns < 70%" mixes ceiling and floor phrasing). Resolution, as utilization CAPS on the projected (p95) utilization of the candidate class: CPU < 60%, IOPS < 60% ("IOPS headroom above 40%"), memory < 75% ("free memory above 25%"), connections < 70%. All four are constants in the DB analysis config, so the exact numbers are a policy knob, not code. The golden fixtures are hand-computed against these values.

**Consequences:** The M3 surface is a superset of the M2 shapes — no new tables, no parallel API (DB recommendations appear in the existing lists with `Resource="class"`), and the dashboard's unified surface filter falls out of the existing data. The cost is that `Workload`, `Recommendation`, and `Diff` carry fields that are meaningless for k8s workloads (documented as such in the structs). The maintenance-window guardrail and the approval default are enforced in the DB apply path only — k8s applies keep their existing auto-apply label policy (ADR-018). Live provider collectors land as thin `dbmetrics.Source` implementations later, like GCP pricing did.

## ADR-031: DB apply guardrails — maintenance windows, one class step, approval default

**Status:** Accepted

**Context:** ADR-030 unified databases into the existing model but left the DB write surface's safety semantics to the apply engine. The k8s apply engine's guardrail philosophy (store health, pending-only, exclusions win, mode policy, concurrency, audit-first) carries over unchanged; the DB surface adds its own failure modes that k8s doesn't have: instance changes are provider operations with their own timing constraints, and class changes are absolute writes (the target class names the full target state — unlike k8s diffs, which are relative to live).

**Decision:**

1. **Maintenance window (weekly, UTC, `ddd:hh:mm-ddd:hh:mm`), enforced on every real apply.** `end < start` wraps past midnight (`sun:23:00-mon:01:00`); `end == start` is malformed; an empty or malformed window is **fail-closed for every mode** — an instance with no configured window can never be changed. Dry-runs are exempt from the *timing* guard (planning ahead is the point of a dry-run) but still report the window state (`InWindow`/`Window`) in the response, and still fail closed when the window is unconfigured or malformed.
2. **One class step per apply.** A recommendation may span several catalog classes (e.g. xlarge → micro); each apply moves exactly one adjacent catalog step and queues the remainder as a follow-up pending recommendation (the follow-up's current class is the class just applied, its savings the price difference it will close). Same stepping philosophy as the k8s ≤30% rule — every change is a small, verifiable increment. Downsize-only, like the k8s policy.
3. **Approval is the default.** `mode=auto` on the DB surface requires the instance label `consize.savings.dev/auto-db=enabled` (the plan's opt-in flag); `mode=approved` requires an explicit actor; `mode=dry_run` records a planned event and touches nothing. A dry-run never queues follow-ups — it must not mutate the recommendation set.
4. **The store row is not written by the apply engine.** The k8s engine doesn't update the workload row either — the collector's next sync converges live state into the store. The DB engine mirrors this exactly: the follow-up recommendation carries the applied class forward, and the store's `DBClass` converges when the DB collector (ADR-030 §8) lands. Rollback therefore needs no live read: the pre-apply class in the apply event is the rollback target (ADR-032).
5. **`ClassChanger` is the provider seam, `StubChanger` the shipped placeholder.** The API (`cmd/api`) and verifier (`cmd/verify`) both construct the DB engine with the stub, which fails every real write with an explicit "manual class change required" message — a FAIL verdict then escalates to manual intervention instead of silently doing nothing. Dry-runs and guardrail outcomes work end to end without a provider.
6. **The API routes on the recommendation's resource** — `Resource="class"` → DB engine, cpu/memory → k8s engine — one write surface per kind, decided at the same point the engines enforce it. A fully read-only API (neither engine) answers 503 for the whole apply surface; a missing engine answers 503 for its kind. Guardrail blocks come back as structured 422 `{"error":"apply blocked","reasons":[...]}`.

**Consequences:** The DB surface cannot be applied accidentally: no window → blocked; outside the window → blocked; automatic mode → blocked without the label; a multi-step move → one step at a time with an audit trail per step. The maintenance-window rule is the DB counterpart of the k8s auto-apply label policy (ADR-018) — enforced in the DB path only. The stub provider keeps the endpoint honest until a live CloudWatch/Cloud Monitoring integration lands.

## ADR-032: DB verification judges against absolute caps on the applied class

**Status:** Accepted

**Context:** ADR-030 §7 says the DB verifier reads store buckets, not a live provider — but left the judgment semantics open. The k8s verifier compares the post window against a baseline-relative multiplier (e.g. 1.5× baseline). That design would be wrong for DB downsizes: a class downsize *legitimately* raises utilization (same demand, fewer resources), so a relative threshold would false-positive on every healthy downsize.

**Decision:**

1. **Judgment is against the analysis caps, absolutely** (CPU < 60%, connections < 70% of the applied class's baseline, sustained per the shipped 5-minute rule, measured in 15-minute collector steps — DB buckets are 15 minutes, so one bucket at/above a cap is a 15-minute breach). The threshold is the cap, not baseline × multiplier; the baseline window still provides evidence (medians) and the one-window-missing inconclusive rule (ADR-006/027) still applies.
2. **`≥` semantics at the cap.** The analysis promise is "projected p95 *below* the cap", so a bucket exactly at the cap is a breach (`dbLongestRun` uses ≥, where the k8s path uses strictly-greater).
3. **Connections are projected onto the class that was actually applied**, not the recommended one: values are absolute counts in the store (ADR-030 §2), so the verifier divides by the applied class's catalog baseline. Judging against the wrong class would pass a regression (1800 connections = 75% of a `db.t3.large` baseline but 56% of `xlarge`).
4. **The error counter is judged per-bucket median, not window totals.** Windows have different lengths (24 h baseline vs a fresh post window); summing would compare apples to oranges. A sustained rise in errors-per-bucket is the regression signature (`post median > baseline median` fails).
5. **`Verify` dispatches internally**: a class event can never be judged by the k8s path (it would query container metrics and roll back through the deployment patcher) and vice versa — the resource tag on the event is the decision.
6. **Rollback restores the absolute pre-apply class from the apply event** — never a delta, never a live read. Class writes are absolute, so the recorded `ClassCurrent` is the complete rollback target even if the live class drifted during the window (same rationale as ADR-026). The reverted event records the inverted class pair.

**Consequences:** A healthy downsize passes (utilization rises but stays under the caps it was sized for); a real regression — sustained saturation at or above a cap, connections past the applied class's capacity, or more errors per bucket — fails and auto-rolls-back. With no store metrics at all the verdict is inconclusive, never a pass (ADR-006). The DB path needs no Prometheus client: its only data source is the store, so the verifier's DB tests run with a nil client.

## ADR-033: DB metrics fixture and collector wiring (demo seed)

**Status:** Accepted

**Context:** ADR-030 §8 defined the collector-side `dbmetrics.Source` seam and deferred live cloud adapters. For tests and the live-cluster demo the seam needs a shipped implementation, and the collector needs a defined way to ingest a database surface alongside the k8s one — without forcing every deployment to configure one.

**Decision:**

1. **`internal/dbmetrics` ships a deterministic `Fixture` source.** It owns one RDS-style instance (`payments-prod`, `db.t3.large`, namespace `rds`, provider `fixture`) running the hand-computed golden demand — 10% CPU, 12.5% memory, 200 IOPS, 300 connections — modulated by daily (±10%) and weekly (±5%) sinusoids evaluated from the Unix clock. The modulation keeps the dashboard charts alive while the projected p95 stays within a few percent of the base, so the golden recommendation (`db.t3.medium`, $50/mo) holds exactly and remains hand-verifiable. Errors are a constant 2 per bucket so the verifier's error SLI passes on a healthy downsize (ADR-032 §4).
2. **The fixture's maintenance window is `sun:00:00-sat:00:00` UTC** — in-window for every moment except Saturday UTC, so live demo applies work whenever they're run while the guardrail stays demonstrable. Its `consize.savings.dev/auto-db=enabled` label is set, so the approval-default guardrail's auto path is demonstrable too.
3. **The collector gains an optional DB surface** (`Collector.DB`, nil = k8s only), ingested after the k8s path in `Run`: instances upsert as `Source="db"` workloads, and their series land in `usage_buckets` under the five `db_*` metric names — the same idempotent upsert semantics as the k8s path, so re-collecting a window is cheap and duplicate-free.
4. **`CONSIZE_DBMETRICS` selects the source in `cmd/collector`**: unset/`none` = k8s only (the shipped default, no behavior change); `fixture` = the demo fixture. Unknown values fail at startup — a misconfigured surface reports itself instead of silently collecting nothing. Live CloudWatch/Cloud Monitoring adapters land as new cases of this switch later (ADR-030 §8).

**Consequences:** The full DB loop is exercisable end to end without any cloud account: collector (fixture) → analysis (`db.t3.medium` recommendation) → apply (guardrails, one class step, window) → verify (store-bucket SLIs, absolute caps). The live cluster's collector CronJob sets `CONSIZE_DBMETRICS=fixture` (commented as the demo-seed value in `engine/deploy/collector-cronjob.yaml`); production deployments leave it unset until a real adapter exists. The verifier never touches this seam — it reads the store, as ADR-030 §7 requires.

## ADR-034: Live DB provider — CloudWatch RDS adapter and the chart/reporting contract (M3.5)

**Status:** Accepted

**Context:** ADR-030 §8 and ADR-033 deferred live DB providers behind the `dbmetrics.Source` seam; the only shipped implementation was the deterministic fixture. That was the single biggest demo→product gap: a production deployment pointed at a real fleet would ingest **zero** databases. M4's UI also needed a per-workload chart contract and honest savings reporting. This ADR ships the AWS adapter (the market wedge — RDS is the rightsizing target, the class catalog and DB math are RDS-modeled) and the API contract the M4 UI consumes.

**Decision:**

1. **`internal/dbmetrics/cloudwatch` implements `Source` against the RDS/CloudWatch APIs**, hand-rolled with the existing SigV4 (exported from `internal/pricing` as `AWSSigner` — no AWS SDK dependency, matching the repo's dependency-light policy). `ListInstances` pages `DescribeDBInstances` (query protocol, XML); `Series` folds `GetMetricStatistics` (AWS JSON 1.1) into step-aligned `[start,end)` buckets, chunking windows into ≤24 h slices for the 1,440-datapoint API cap. Env config: `CONSIZE_DBMETRICS=cloudwatch`, `CONSIZE_AWS_REGION` (default us-east-1), optional `CONSIZE_DB_FILTER` (comma-separated identifiers), standard AWS credential env vars. Instance fields without a `Workload` home (region, engine, storage) ride as `consize.savings.dev/*` labels; MultiAZ → `DBReplicas=2`.
2. **Metric mapping is honest about CloudWatch's surface**: `db_cpu_percent` ← CPUUtilization, `db_mem_percent` ← 100×(1−FreeableMemory/(catalog GiB×2³⁰)) clamped at 0 (FreeableMemory dips negative on small instances), `db_iops` ← ReadIOPS+WriteIOPS, `db_connections` ← DatabaseConnections. **`db_errors` has no CloudWatch equivalent → the adapter returns no data for it, and the verifier's missing-metric semantics are locked by tests in both directions**: a healthy verification passes with the errors SLI "unavailable", and a CPU-saturation regression still FAILs and rolls back without it. No data is never evidence of health (ADR-006).
3. **`GET /api/v1/workloads/{id}/series?metric=&days=` is the chart contract.** Five metric names regardless of surface (`cpu_percent, mem_percent, iops, connections, errors`; anything else 400; unknown workload 404; `days` positive int, default 14). Resolution is **surface-aware**: DB workloads read the `db_*` store metrics with units percent/iops/connections/errors; compute workloads read the k8s raw metrics (`cpu_used_milli`, `mem_used_bytes`) with units millicores/bytes. A contract-valid name with no store metric on the surface (e.g. iops for a compute workload) is **200 with empty points — no-data, not an error**, like a source that doesn't emit it. The response carries `unit` so the UI labels axes honestly. The initial implementation mapped only DB metrics (found on the live cluster: compute series came back empty while analysis clearly had the buckets); the fix is the surface-aware resolution above, with tests for both surfaces.
4. **`GET /api/v1/savings` gains realized numbers, additive to the existing projected fields** (existing consumers unchanged): `realized_monthly`/`realized_yearly` = sum of `SavingsMonthly` over recommendations whose **latest** apply event is still `applied` **and** whose latest apply has a `passed` verification verdict. A later `reverted` event, dry-run, failed verification, or inconclusive verification excludes it; projected and realized are never mixed. `by_owner` gives the same two numbers per owner label (unassigned when absent). This keeps the M4 AC literal: realized means verified, not merely attempted.
5. **Recommendations gain `risk` (low|medium|high) + `risk_reasons`**, computed at the API from existing data (no schema change): low data days, saturation near headroom constraints, step distance > 1 class, maintenance window not open, follow-up pending, data-loss-risk flags. The UI renders a risk pill with the reasons as tooltip, and ranks by savings.
6. **GCP Cloud Monitoring (Cloud SQL) is the documented follow-up on the same `Source` seam** — one interface, a second implementation (same shape as M1's GCP-pricing deferral). The live-account AWS E2E is a CI gate like the pricing live fetch (ADR-014): the adapter is unit-tested against canned RDS/CloudWatch responses (httptest, hand-computed values); a real account is required for the live gate, and the demo cluster keeps `CONSIZE_DBMETRICS=fixture`.

**Consequences:** A production deployment with AWS credentials now ingests a real RDS fleet end to end — collect → analyze → guarded apply → verify → rollback — with the same safety semantics as the fixture. The M4 UI's charts work for both surfaces with honest units; the savings story is provable ("projected $52.59/mo, realized $0.36/mo from the one verified apply" on the live cluster as of 2026-08-25). The `db_errors` gap is documented, not silent. The compute-series fix is regression-tested. Remaining: the GCP adapter, live-account E2E, and the M4 weekly digest + Grafana tasks (plan.md).

## ADR-035: Live DB provider — GCP Cloud Monitoring (Cloud SQL) adapter, provider-scoped catalogs (M3.5 §6 closed)

**Status:** Accepted

**Context:** ADR-034 §6 recorded the GCP adapter as the follow-up on the `dbmetrics.Source` seam. The user's product focus moved to their real GCP account (2026-08-25 mandate: "don't use seeded data again, I want to focus on the GCP account"), so the adapter shipped before the live-account AWS gate — the fixture demo path is retired from the live cluster entirely. Two facts drove decisions: Cloud SQL only accepts `db-custom-*` or shared-core tiers for Postgres (legacy `db-n1-standard-*` is rejected with HTTPError 400 — learned live), and the Admin API's maintenance-window day convention is **1 = Monday … 7 = Sunday** (verified against `gcloud sql instances describe` on the live instance: a Sunday window returns `day: 7`).

**Decision:**

1. **`internal/dbmetrics/cloudmonitoring` implements `Source` against the Cloud SQL Admin API and Cloud Monitoring.** `ListInstances` pages `sql/v1beta4/projects/{p}/instances` (JSON, state RUNNABLE only); `Series` queries `v3/projects/{p}/timeSeries` with the metric descriptors below, mapped to the five `db_*` store metrics. Auth is hand-rolled: an RS256 JWT minted from the `GOOGLE_APPLICATION_CREDENTIALS` service-account key (RFC 7523, `token_uri`), with the GCE metadata-server fallback for in-cluster runs; `tokenFunc` is injectable for tests. Env: `CONSIZE_DBMETRICS=gcp`, `CONSIZE_GCP_PROJECT` (default: key's `project_id`, then metadata), optional `CONSIZE_DB_FILTER`.
2. **Metric mapping follows Cloud Monitoring's surface**: `db_cpu_percent` ← `cloudsql.googleapis.com/database/cpu/utilization` ×100 clamped 0..100, `db_mem_percent` ← `database/memory/utilization` ×100, `db_connections` ← `database/network/connections`; `db_iops` and `db_errors` have **no Cloud Monitoring equivalent → no-evidence, never FAIL** (same semantics as ADR-034 §2's `db_errors`; the verifier's no-evidence tests cover both adapters). Series requests carry the RUNNABLE filter, paginate with `nextPageToken`, and map days-to-window with the verified Monday-first convention (`gcpDays`); the initial Monday-first bug (shipped mapping 1=Sunday → a Sunday window rendered `sat:03:00`) was found live on consize-demo, fixed, and the mapping table now encodes the real convention. ZONAL→1 / REGIONAL→2 replicas; userLabels + derived `consize.savings.dev/{region,tier,storage}` labels, derived set last so they cannot be clobbered.
3. **DB class catalogs are provider-scoped.** `analysis.DBCatalog` (RDS) and the new `GCPDBCatalog` (price-ordered: db-f1-micro $11, db-g1-small $43, db-custom-1-3840 $72, db-custom-2-7680 $144, db-custom-4-15360 $288, db-custom-8-30720 $575 — documented us-central1 defaults, `VCPU`/`MemGiB` now float64 for GCP's fractional shapes) are **never merged**: a single ladder would let RDS propose GCP classes and vice versa. `DBClassStep`, `dbapply.stepPlan`/`classPrice`, the verifier's `dbClassFor`, and `dbSizing` all resolve within the workload's own provider's catalog; golden tests lock both directions (RDS must not propose GCP tiers; a GCP instance must step within GCP tiers).
4. **The collector's `CONSIZE_DBMETRICS` switch gains `gcp`** (`dbSourceFor` refactor makes the switch unit-testable: fixture/cloudwatch/gcp/unknown). The live cluster's collector CronJob now runs `CONSIZE_DBMETRICS=gcp` with the service-account key mounted from a `consize-gcp` Secret (`GOOGLE_APPLICATION_CREDENTIALS=/etc/consize-gcp/key.json`); `consize-analyze` runs `CONSIZE_MIN_DATA_DAYS=0.1` as a documented bootstrap value for the fresh instance (restore the default 5 once it holds 5 days of data). The fixture workload was deleted from the live store — the demo seed no longer exists in any live path (poke and cluster).

**Consequences:** The live cluster ingests the user's real Cloud SQL instance end to end: consize-demo (`db-custom-1-3840`, ZONAL, window sun 03:00–04:00, us-central1) → real Cloud Monitoring series → recommendation `db-g1-small` $29/mo with honest risk (medium: low data days, window not yet open). Projected savings on the cluster are now 100% real: $31.59/mo projected (incl. the $29 GCP rec) + $0.36 realized from the verified M2 apply. The Monday-first convention is locked by tests; a wrong window would have let applies through on the wrong day. Remaining on this seam: the live-account AWS E2E as CI gate (unchanged, ADR-034 §6).

## ADR-036: Next.js product UI (M4 rewrite) — product-standard architecture, embedded SPA retained as fallback

**Status:** Accepted

**Context:** The M4 UI shipped as a vanilla static SPA embedded in the API binary (ADR-034 §4 area) — demoable, but not product-standard: no component model, no SSR, no typed API layer, hard to extend. The user's 2026-08-25 mandate: "why are we not using nextjs? … make the UI standard, have you seen usage.ai?" — benchmark is a dark FinOps console (near-black canvas, sidebar navigation, KPI cards with dollar deltas, status pills, uppercase micro-labels). The engine API is already a clean `/api/v1` contract (ADR-034 §3), so a modern frontend can sit on it without engine changes.

**Decision:**

1. **`ui/` is a Next.js (App Router, TypeScript, Tailwind) application** at the repo root, replacing the embedded SPA as the product UI: `lib/api.ts` typed client against relative `/api/v1`, `lib/types.ts` API types, `lib/format.ts` currency/number formatting, `components/` (Sidebar, ApplyModal, ApplyTimeline, UsageChart, ui primitives), `app/` routes (dashboard, workloads, workload detail, recommendations, audit, apply). Charts: Recharts; icons: lucide-react.
2. **`next.config.ts` rewrites `/api/v1/:path*` → `API_UPSTREAM`** (default `http://127.0.0.1:18099`) so the same build runs against the local poke, the cluster, or a cloud-deployed API — the frontend never hardcodes a backend origin; `NEXT_PUBLIC_API_BASE` remains the escape hatch. CORS is avoided entirely by same-origin proxying.
3. **The embedded SPA stays in the API binary untouched** as the single-binary fallback (zero-dependency demo: one image, one port). It is not developed further; new UI work lands in `ui/`.
4. **Deploy story** (ADR-035 §4 context): `ui/` builds to a static export or Node server image alongside the engine images; the cluster serves it through the same `consize-api` Service (route `/` → UI, `/api/v1` → engine) once the rollout lands. Until then the poke runs `next dev`/`next start` against the local API.

**Consequences:** The product UI has a standard stack a hiring-team reviewer would expect (Next.js 16, App Router, typed API client), matches the usage.ai-class visual benchmark, and shares one API contract with the embedded SPA. Cost: a second deployable and a build step the single binary didn't have; the engine remains the single source of truth for all data and safety decisions (the UI stays a read-mostly client; applies still require the engine's guardrails).

## ADR-037: Authentication and server-side authorization — local users, revocable sessions, role-gated writes

**Status:** Accepted

**Context:** Until now the API had no identity layer at all: the apply endpoint's `actor` was a client-supplied JSON body string validated only for non-emptiness, `mode=auto` needed no identity, and every read was open. That is the documented M4 acceptance-criterion gap ("UI read-only users cannot trigger applies; RBAC enforced server-side, not just hidden buttons"), and security.md §2/§8 already promised the target model (OIDC, roles consize:view / consize:operator / consize:admin, server-side enforcement contract). This ADR delivers the local-user version of that contract with a provider seam that OIDC can implement later.

**Decision:**

1. **Local users with three roles** (`viewer` read-only, `operator` can approve and apply, `admin` everything), stored in a `users` table (email unique, bcrypt password hash, role CHECK). Roles map one-to-one to security.md §2's consize:view / consize:operator / consize:admin; `roleAtLeast` orders viewer < operator < admin so `RequireRole("operator")` admits admins too.
2. **Postgres-backed, revocable sessions** (`sessions` table): the client gets a 32-byte token exactly once; only its SHA-256 hash is stored, expiry is a table column (7-day TTL), and logout is a row delete. The sessions table is therefore not a credential store and a leak cannot be replayed. No JWT dependency — matches the repo's zero-dependency hand-rolled style; bcrypt comes from `golang.org/x/crypto` (the first new direct dependency since the stack was built).
3. **`internal/auth` package with an `Authenticator` seam.** `Service` (LocalUsers) implements `Authenticate(ctx, token) → user`; the API and the chi middleware depend only on the interface, so an OIDC provider slots in as a second implementation without touching the API (the repo's seam-first pattern, cf. `dbmetrics.Source` ADR-030 §8, `ClassChanger` ADR-031 §5). Middleware: `RequireUser` (resolves the `consize_session` httpOnly cookie into the request context; 401 `{"error":"unauthorized"}`) and `RequireRole` (403 `{"error":"forbidden","role_required":…}`), composed per route group.
4. **Wire contract:** `POST /api/v1/auth/login` (one 401 for both an unknown email and a wrong password — the endpoint must not reveal which), `POST /auth/logout` (idempotent, clears the cookie), `GET /auth/me` (`{"auth_enabled":…,"user":…}` — `auth_enabled:false` explicitly distinguishes "auth not enforced" from "not logged in" for the client). Reads require a session; applies require operator+; login/logout are public. **The apply `actor` is server-verified**: with auth enabled the handler overwrites any client-supplied actor with `api:<session email>` — the audit-trail comment `Actor string // operator | auto | api:<user>` is now literally true.
5. **Auth is an opt-in additive option.** `api.New` gains a variadic `Options{Auth *auth.Service, CookieSecure bool}`; zero options = auth disabled = the exact pre-ADR-037 behavior, so every existing test and the embedded-SPA fallback keep working. Middleware receives the nil-interface explicitly (a nil `*auth.Service` boxed into the `Authenticator` interface is not nil — a real bug the existing test suite caught).
6. **Bootstrap admin:** `CONSIZE_BOOTSTRAP_ADMIN="email:password"` creates the first admin only while `users` is empty, then is ignored forever (one-time out-of-band fact). `CONSIZE_AUTH_REQUIRED` defaults **true** (enterprise posture); the demo builds (embedded SPA fallback, curl actor flows) opt out explicitly. `CONSIZE_COOKIE_SECURE` marks the cookie Secure behind TLS.
7. **UI:** a `/login` page (dark FinOps shell), an auth context probing `/auth/me` on load, a route guard bouncing unauthenticated sessions to `/login` and rendering `/login` without the sidebar, a sidebar session block (email, role, logout), and role-aware apply gating — viewer hides the Apply button *and* the server rejects the call (the AC's "not just hidden buttons"). The ApplyModal hides its actor field when auth is enforced (the server derives it).

**Consequences:** Nobody can trigger an apply without a server-verified identity and role; the audit trail records who the server knows acted. The poke runs auth-enforced with a bootstrap admin; the live cluster runs the new image with `CONSIZE_AUTH_REQUIRED=false` for now (embedded-SPA fallback and curl actor flows stay functional) — flipping to true plus a `consize-auth` Secret (bootstrap-admin) turns enforcement on; the migration is already applied on the cluster DB. M4's auth AC is closed; the security.md §8 contract test shape (read-only token cannot POST apply) is covered by `TestAuthHandlerMatrix` and `TestActorIsServerVerified`. Remaining on this seam: OIDC SSO (E1.5 onboarding), rate limiting and approval tokens (§7 backlog), and the API-key/agent identity story for Terraform-PR applies (E2.4).

**Amendment (2026-08-26, §6): first-run admin creation.** Ad-hoc deployments (the poke, any future self-hosted install) get an interactive equivalent of the bootstrap env var: `POST /api/v1/auth/setup` creates the first admin while `users` is empty (409 forever after), gated by the same `CountUsers` one-admin-ever rule as `CreateBootstrapAdmin` — the first to run wins. The `/auth/me` 401 body now carries `needs_setup:true` while the table is empty, so the UI renders a first-run wizard on `/login` ("Create admin & sign in") instead of the login form; the wizard enforces a minimum 8-character password server-side (422). There is deliberately **no default credential anywhere** (Grafana-style admin/admin was rejected for a production-mutating tool) and no open registration: once one admin exists, the wizard is gone and the only way in is a session. Covered by `TestFirstAdminSetup` (advertise → 422 weak → 200 admin → 409 → login) and a 10/10 CDP smoke against a fresh empty-users stack. Deploy note: an env-var bootstrap still wins for scripted deploys (the wizard only appears when both are absent); the cluster currently needs neither because auth is disabled there.

## ADR-038: Branding — conSize wordmark, brand mark, favicon

**Status:** Accepted

**Context:** The UI shipped with a generic app icon and a plain text sidebar header ("consize" all-lowercase). As the product polish pass began (E1.6, user's 2026-08-26 reorder: "let's do the branding now … improve on the UI and logo"), the brand needed a consistent, token-driven identity: one logo component every surface renders from, a distinctive favicon, and the wordmark's marketing spelling ("conSize", capital S) carried through the product name in title and chrome.

**Decision:**

1. **A single `components/Brand.tsx` is the only brand renderer** (sizes `md` sidebar / `lg` login, optional "Rightsizing" micro subtitle). It composes the **brand tile** — a rounded square with a radial green wash, a panel-2→bg-soft gradient, a 1px green ring and inset highlight, carrying the Gauge icon in the brand green — with the wordmark `conSize` (capital S in `text-green`, the brand color token). Every surface (sidebar, login page, loading/signing-in frames) renders this one component, so logo variants cannot drift.
2. **The brand tile derives from palette tokens** (`.brand-tile` in globals.css), so light mode (ADR-040) re-themes it with the same tokens — no separate light logo.
3. **Favicon is a gauge mark** (`app/icon.svg`, 64×64: dark rounded square, green ring arc, green needle — the same gauge motif as the brand tile). `favicon.ico` is removed; Next serves `/icon.svg` automatically and the page metadata title is "conSize — infrastructure rightsizing".
4. **No asset pipeline**: the logo is markup + one inline SVG, zero image downloads, crisp at any density — consistent with the dependency-light product policy.

**Consequences:** The product identity is a single render path (no pixel drift), theme-aware by construction, and the gauge motif now runs through tile, favicon, and wordmark. Verified by the polish smoke (33/33): wordmark + green capital S on login and sidebar, RIGHTSIZING micro, `link[rel=icon]` → `/icon.svg`, and the SVG's gauge path fetched and asserted.

## ADR-039: Improved navigation — grouped sections, ⌘K command palette, mobile off-canvas drawer

**Status:** Accepted

**Context:** The first-product sidebar was a flat link list with no hierarchy, no keyboard path, and no mobile story (it pinned left at every width). E1.7 (user's reorder: "then the improved navigation") upgrades the frame to a product-standard shell: grouped sections, a keyboard-first jump-to-anything palette, and a proper mobile drawer.

**Decision:**

1. **Grouped navigation** — three sections with uppercase kickers: Overview (Dashboard), Optimize (Recommendations, Workloads), Operations (Audit). The active route is marked with `aria-current="page"` plus the existing green inset bar; `onClick` closes the drawer.
2. **⌘K command palette** (`components/CommandPalette.tsx`, mounted once in Shell so it exists on every page): ⌘K / Ctrl+K toggles, ↑/↓ move the selection, Enter jumps, Esc or backdrop click closes. Static routes always listed; the **workload index is fetched once, lazily, on first open** (`api.workloads()`, failures degrade to routes-only) and typed queries surface up to 8 workloads by name with source hints (`compute · namespace` / `database · provider`) and small provider-colored avatars — jump to any workload detail from anywhere. Empty query = routes only, so the palette is never noisy.
3. **Mobile off-canvas drawer** — below `lg`, the sidebar is `-translate-x-full` (transform-transitioned, `z-40`), toggled from a new sticky top bar (`lg:hidden`, hamburger + compact brand + theme toggle) and closed by tapping the backdrop (`z-30`), by clicking a nav link, or automatically on route change (Shell owns the `navOpen` state). Content area gains `lg:pl-[232px]` to match the fixed sidebar.
4. **Footer affordances** — the safety-engine card now carries a `<kbd>⌘K</kbd> jump to anything` hint; the session block is unchanged.

**Consequences:** Navigation is sectioned and scannable on desktop, keyboard-complete (⌘K for anything, incl. live workloads), and the app is usable on a phone-width viewport. The palette's workload surface rides the existing typed API client — no new endpoint. Verified by the polish smoke: sections rendered, active state marked, palette opens with 4 routes, a real workload name typed in-page surfaces its row and Enter lands on `/workloads/<id>` (inference-api on the live poke), Esc closes, and at 390 px the hamburger opens the drawer, a link click navigates and closes it.

## ADR-040: Light/dark mode — next-themes, attribute-switched token palettes

**Status:** Accepted

**Context:** The console shipped dark-only (color-scheme: dark, all palette tokens in `:root`). E1.8 (user's reorder: "then toggling of light and dark mode etc.") adds an explicit user-facing toggle; the entire UI already flows from CSS variables, so theming is a token problem, not a restyle problem.

**Decision:**

1. **`next-themes` with `attribute="data-theme"`**, default `dark`, `enableSystem={false}` — the console is dark-first by design (usage.ai-class FinOps look); the user's choice persists in localStorage and the pre-hydration script stamps the attribute so there is no flash. `suppressHydrationWarning` on `<html>`.
2. **One light palette block, zero component changes**: `:root[data-theme="light"]` redefines every raw token (backgrounds `#f5f7fa` canvas / white panels, near-black ink, slate borders) and re-tints the accent set for contrast on white (`--green #059669`, `--red #dc2626`, `--amber #b45309`, `--blue #2563eb`, `--teal #0d9488`, dims re-derived at low alpha). Because `@theme inline` maps utilities through the vars, panels, charts, pills, the brand tile, and scrollbars all re-theme together.
3. **Three toggle surfaces, two shapes** (one `ThemeToggle` component): a labeled Dark/Light segmented control in the sidebar footer; icon buttons (Sun in dark / Moon in light) on the mobile top bar and the login page top-right — reachable before signing in. SSR-safe: `resolvedTheme === undefined` treats as dark until hydration.

**Consequences:** The entire console is light-capable with a single token block; the brand tile re-themes without a second logo (ADR-038 §2); charts and pills keep contrast on white. Verified by the polish smoke: dark default body `rgb(11,15,20)`, Light seg flips to `rgb(245,247,250)` + persists across reload, Dark flips back, and the login-page icon toggle flips both ways.

## ADR-041: Visual standard — Zorveus-standard monochrome restyle (Geist, pure black, one accent)

**Status:** Accepted

**Context:** The first polish pass (ADRs 038–040) shipped the wordmark, the command palette, the mobile drawer, and theming — but the look itself was judged substandard against the reference the user set: *"all you just did is very poor, look at Zorveus.com, I want that standard."* Zorveus (a Vercel-school SaaS site) was therefore **measured, not guessed**: full-page screenshots plus computed-style extraction established the concrete standard this ADR encodes. The structural decisions of ADRs 038–040 (single Brand render path, palette-driven chrome, ⌘K navigation, next-themes switching) all stand; this ADR replaces the *look* those components render.

**Measured reference (zorveus.com, 2026-08-26).** Geist-style grotesque type; near-black canvas with #0e0e0e–#0a0a0a surfaces on pure black; white text ≈ #fafafa and muted grays ≈ #a1a1a1; hero H1 60px/600 with −1.5px tracking; card bg ≈ #141414, radius 14–18 px, 1px white/10% borders; layered shadows `0 2px 2px rgba(0,0,0,0.04), 0 24px 48px -12px rgba(0,0,0,0.08)`; solid primary buttons with ghost secondary; pill badges (translucent, 12px); circular letter avatars; monochrome with exactly one restrained accent.

**Decision:**

1. **Geist replaces Inter** — Vercel's grotesque, the family Zorveus renders in. Loaded via `next/font/google` as `--font-geist` (layout.tsx comment notes the metrics-compatible Inter fallback if the fetch fails at build) and wired into the `--sans` token; the whole console's type follows the token, so nothing else changes.
2. **Pure-neutral palette.** Dark: canvas `#000000`, surfaces `#0a0a0a`, panels `#141414`/`#1d1d1d`, ink `#fafafa`/`#d4d4d4`, muted `#a1a1a1`, faint `#737373`; borders are white-alpha (`rgba(255,255,255,0.08)` / `0.16`). Light (**superseding** ADR-040's slate `#f5f7fa` palette): canvas `#f7f7f7`, white panels, ink `#111111`, muted `#565656`, black-alpha borders. All colors flow through `:root` / `:root[data-theme="light"]` tokens → `@theme inline`, so every surface re-themes from one block.
3. **One accent.** Green `#34d399` is the single brand accent — primary buttons, active states, brand tile, focus/selection tints — in both themes. The data-semantics accents (red `#ff5c5c`, amber `#ffb224`, blue `#6ba7ff`, teal `#3ee0c4`) are re-brightened for black and dimmed at 0.14 alpha.
4. **Vercel-style cards.** `.card` is `linear-gradient(180deg, var(--raise), var(--panel))`, radius 14 px, with the layered Vercel shadow. The gradient top is a **new `--raise` token** (`#181818` dark / `#ffffff` light) so card faces re-theme with everything else instead of a hard-coded dark-only highlight. `.stat` KPI cards carry the same treatment with 34 px values at −0.025em.
5. **Buttons.** Ghost is the default (transparent, white/12% border, radius 10); `.primary` is solid green with a `#052e21` label and green glow (`0 4px 20px rgba(52,211,153,0.22)`), hover brightening — Zorveus's solid-primary / ghost-secondary pairing in the brand color.
6. **Zorveus chrome details.** Avatars become circular (`border-radius: 999px`); kickers become pill badges (translucent fill + 0.22-alpha tinted border); body type 14.5 px / 1.55; H1 28 px / 700 / −0.02em; section kickers keep 0.12em tracking; selection color green at 0.3 alpha. The login page gains the Zorveus hero treatment — a pill kicker ("Infrastructure rightsizing") and an ambient green radial glow behind the brand.
7. **Scope discipline.** Presentational only: Geist wiring in layout.tsx, the login hero, two hover-class fixes (`hover:bg-white/10` where slate-alpha had been hard-coded), the KPI-grid gap, and the token block. No layout, behavior, or data changes.

**Consequences:** The console now renders the measured Zorveus standard — pure-black canvas, #141414 cards with Vercel shadows, white-alpha borders, one green accent, Geist type — in both themes from the token system alone. ADR-040's light palette values are superseded by §2 (the switching *mechanism* stands); ADR-038's brand components are unchanged and simply re-render. Verified at the pixel level, not only by computed styles: screenshots captured with the theme pinned in-page (`data-theme` plus body `rgb(0,0,0)` / `rgb(247,247,247)` asserted before every shot) were decoded with Chrome's own canvas decoder — control-tested against the known-good Zorveus screenshot (samples `#0e0e0e`) — and sample exactly the token values (`#000000` canvas, `#0a0a0a` sidebar, `#141414` card edge, `#ffffff` light panels). Screenshots: `/tmp/ui-smoke/consize-new-{login,dashboard,dashboard-light}.png`. Smokes re-run with the new pixel assertions: polish 33/33, auth 10/10.

**Amendment 1 (2026-08-26, user clarification): "I didn't say copy it exactly, just make it like that standard."** The standard is a quality bar, not pixel parity. Same-day course-corrections from a live re-measurement of zorveus.com: the primary CTA was briefly switched to Zorveus's solid white (measured lab(98.26)/lab(4.06), radius 10, flat) and **reverted to the brand green the same day** — the green primary is the product identity (ADR-038), the white CTA was copying, not standard-making. `--radius` moved 14 → **18 px** (Zorveus's measured card radius, inside the announced 14–18 range), and the modal/palette/safety radii were wired to the token so the whole card language is one knob. Also per user directive, **53 unnecessary comments were removed from the UI source** (narrative header blocks, ADR citations, self-evident inline notes); the ~50 kept carry information the code cannot show — enum value lists, wire-format notes, race-guard and server-contract explanations. Verification re-run after both changes: build clean, polish smoke 33/33, auth smoke 10/10, screenshots re-captured with pinned themes and pixel-checked (`#000000` canvas, `#0a0a0a` sidebar, `#141414` card edge, `#ffffff` light panels, button computed `rgb(52,211,153)`).

**Amendment 2 (2026-08-26, user directive — "it makes the whole thing look AI-generated"):** five marketing/explainer copy blocks were removed from the product UI; the copy is reserved for the future landing page (noted in plan.md backlog): (1) the sidebar safety-engine card ("Safety engine / Every change is analyzed, guarded, verified — and reversible. Analyze → Guarded apply → Verify → Auto-rollback → Audit" + the ⌘K hint), (2) the dashboard's 5-step safety strip and (3) its subtitle ("Rightsizing with a safety engine — every recommendation is applied, verified, and audited."), (4) the login hero pill ("Infrastructure rightsizing") and (5) the login footnote ("Sessions are server-verified · roles viewer / operator / admin"). The functional first-run wizard message ("No accounts exist yet — the first account becomes the workspace admin") stays. Dead CSS followed the copy: the `.safety*` strip block and the sidebar-only `.kbd` block were removed from globals.css, and `PageHead.sub` became optional — the dashboard renders title-only; the data speaks. Re-verified after the edit: build clean; polish smoke 33/33 and auth smoke 10/10 re-run; screenshots re-captured with pinned themes and pixel-checked against the token values; and a live-DOM absence check confirms none of the five strings render on the login page or the dashboard.

## ADR-042: Premium dashboard refinement — flat ultra-dark surfaces, emerald accent reservation, new brand lockup

**Status:** Accepted

**Context:** A frontend-redesign brief (2026-08-26) targeting "default component-library aesthetics and heavy vibe-coded dark-mode tropes" supersedes specific ADR-041 choices. The brief's constraints, applied faithfully: pitch-black app background, ultra-dark cards separated by hairline borders instead of heavy fills, a muted emerald reserved for the active sidebar item and positive financial metrics only, pure white for primary data, zinc-500 uppercase micro-type, fully-rounded translucent pills, roomier right-aligned tables, and a clean single-line brand mark. Where the brief is silent, ADR-041 stands (Geist, 18 px radius, token architecture, light theme).

**Decision:**

1. **Brand lockup (supersedes ADR-038 §2 + ADR-041 §6's tile).** The squircle gauge tile, the stacked RIGHTSIZING sub-line, and the green capital S are gone. The mark is now a single-line **conSize in Geist extrabold** preceded by a minimal **emerald diamond** vector (18 px sidebar / 26 px login). The favicon (`/icon.svg`) matches — emerald diamond on pure black (was the gauge-in-squircle).
2. **Surfaces.** `--panel` and `--raise` drop to `#0a0a0a` (cards, palette, modal, timeline), `--border` to white/5% — cards are flat ultra-dark surfaces separated by a hairline, **no gradients, no layered shadows** (`--shadow-card: none`; supersedes ADR-041 §4's `#141414` + Vercel shadow). `--panel-2` → `#141414` (interactive thumbs only). Light theme mirrors: white panels, black-alpha hairlines, shadows off.
3. **Palette.** `--green` #34d399 → **muted emerald `#10b981`** (light keeps #059669). `--ink` #fafafa → **pure white `#ffffff`**; `--faint` → `#71717a` (zinc-500); `--muted` stays #a1a1a1 (zinc-400).
4. **Accent reservation (supersedes ADR-041 §5 + Amendment 1's green-primary identity).** Emerald appears **only** on: the active sidebar item, positive financial metrics (projected/realized KPI values, savings-by-owner cells, recommendation SavingsMonthly cells — `.money`), and the brand mark. The **primary CTA is solid white** (near-black text; black in light theme) — the green primary button is gone. Decorative green was swept: login ambient glow → white-alpha, sidebar avatar circle → white/10, link hovers → white, palette selection → white/10 + white inset bar, spinner → white, input caret → ink. **Functional/status colors kept** (these are interaction feedback and M2 safety semantics, not decoration): input focus border, selection tint, semantic pill tints, timeline dots, maintenance-window text, modal success/error.
5. **Micro-typography.** `.micro` (stat labels, section titles) and table headers: uppercase, **10 px, tracking 0.1em (tracking-widest), zinc-500**. Applied via `--faint`; `.nav-section` aligned to the same tracking.
6. **Pills.** Neutral pills (planned, pending, superseded, n/a) are **rounded-full, `rgba(255,255,255,0.10)` background, pure white text** — the gray fill is banned. Semantic pills (applied/verified/passed → emerald tint, failed/rolled_back → red tint, risk low/med/high → emerald/amber/red) keep their translucent tints: pass/fail must stay legible at a glance (the M2 safety-engine contract), so the ban applies to neutral gray fills, not status color.
7. **Tables.** Headers: 10 px zinc-500 uppercase, padding 12/16/10. Cells: padding 14/16, body text muted (zinc-400), **numeric cells right-aligned, tabular, pure white**, savings cells emerald (`.money`). Hover stays white/3%.
8. **Typography stack.** Geist was already in place (ADR-041 §1) — the brief's "Inter, Geist, or system-ui" was already satisfied; no change.

**Consequences:** The console reads as a flat, monochrome, emerald-accrued instrument: pure-black canvas, hairline-separated ultra-dark cards, white primary data, emerald only where money and the active route live. Verified: build clean; polish smoke updated to the new contract and green — **35/35** (single-line lockup with no RIGHTSIZING sub-text, solid-white primary button, emerald active-nav and savings-KPI computed styles, diamond favicon, plus the pre-existing 32 checks); auth smoke re-run **10/10**; screenshots re-captured with pinned themes and pixel-checked with Chrome's own decoder: login canvas `#000000`, white-alpha glow, disabled (45%) white primary = `#797979` over the card, flat card surfaces `#0a0a0a` (dark) / `#ffffff` (light), sidebar `#0a0a0a`/`#ffffff`. The removal of ADR-041's green-primary identity and card shadow is intentional per the brief's accent-reservation rule; both are token-level changes if the direction ever needs revisiting.

## ADR-043: Teams and on-call ownership — durable workload stewardship

**Status:** Accepted

**Context:** Consize can safely recommend, apply, verify, and roll back changes, but a failed verification previously named only the workload. A production tool needs to show who owns that workload and which escalation target should act. Labels are collector-owned and can drift; ownership therefore cannot live only in Kubernetes metadata.

**Decision:**

1. **A team is the ownership boundary.** The new `teams` table has a stable, generated slug plus a display name, required named owner, and required on-call contact. Contacts are intentionally opaque strings: an email, `#slack-channel`, or incident-management schedule can work without binding the engine to a single external provider.
2. **A workload has zero or one team.** `workloads.team_id` is nullable (`ON DELETE SET NULL`). Assignments are made only through explicit admin APIs: `POST /teams`, `PATCH /teams/{id}`, `PUT /workloads/{id}/team`, and `DELETE /workloads/{id}/team`. Team identity is immutable in this first slice; only owner/on-call contacts change.
3. **Collector safety wins.** The collector's workload upsert never writes `team_id`; both stores preserve an operator-assigned team through every metadata refresh. This avoids an ownership outage caused by a normal collection run.
4. **Visibility is broad; configuration is admin-only.** Any signed-in user can read `/teams` and workload ownership fields. Creating teams, changing contacts, or assigning workloads requires the existing `admin` role; those operations are governance configuration, not routine applies.
5. **Alerts carry ownership, not a new delivery system.** Failed-verification and rollback-failed messages include team and on-call information. The configured Slack webhook remains the delivery path; provider-specific per-team routing is deferred until a PagerDuty/Slack-router integration exists.

**Consequences:** Workloads, their detail page, and a Teams operations view now show the responsible team and escalation target. Admins can create and edit teams and assign/unassign workloads in the UI. Failed verification alerts tell the receiver who should respond. The complete Go suite passes and the Next.js Webpack production build passes; store/API tests specifically cover authorization, assignment, contact updates, and collector-upsert preservation.

## ADR-044: Installation-scoped incident routing and on-call ownership

**Status:** Accepted for backlog (2026-08-27)

**Context:** Consize installations are owned and scoped by one team. A free-form
on-call string is useful context but cannot resolve schedules, page the current
responder, deduplicate repeated failures, or show acknowledgement and
assignment state. A manually managed in-product team directory would duplicate
the organization's incident-management system and drift from its schedules.

**Decision:** Treat the external incident-management service as the source of
truth for incident lifecycle and the Slack channel as the collaboration
surface. Consize emits a provider-neutral incident event through an
`IncidentSink` seam. The first adapters are Slack webhook delivery plus
PagerDuty Events API and Jira Service Management operations alerts. Each
installation configures a provider routing reference and a Slack channel;
provider credentials are mounted by Kubernetes Secret references. Slack
mentions use stable Slack user/group IDs, never display names. Events carry a
stable installation/workload/failure-episode deduplication key and are
idempotent on retries.

Consize stores a local incident projection (provider ID, status, assignee,
timestamps, links, delivery attempts, and errors) for dashboard visibility.
Signed provider webhooks synchronize acknowledgement, assignment, escalation,
and resolution with replay protection. A successful post-rollback verification
may resolve the incident; all other resolution paths are explicit and audited.
The dashboard surfaces the current owner and required change, but incident
ownership never grants apply permission: existing server-side operator/admin
RBAC remains authoritative.

**Consequences:** On-call routing follows the organization's real escalation
policies instead of a stale Consize contact field. PagerDuty can route an
event to its escalation policy and synchronize incident actions into Slack;
Jira Service Management exposes equivalent alert assignment, responder,
acknowledge, and close operations. Slack-only installations still receive
rich, tagged messages, but schedule resolution is intentionally delegated to
the configured provider. This replaces the E1.2 manual team directory for
single-team installations; the existing `teams` slice remains provisional
until the installation model is implemented.

**Amendment (2026-08-28, first slice):** The alert layer now follows the
Grafana Alerting shape directly: Consize emits labeled alert events,
notification policies match those labels, and matched policies deliver to
named contact points. Slack webhook delivery is the first contact-point
integration and uses Block Kit with stable Slack user/group mentions when
configured. `CONSIZE_SLACK_WEBHOOK` remains a backwards-compatible default
contact point; `CONSIZE_ALERT_ROUTING` is the policy-driven path. PagerDuty,
Jira Service Management, durable incident projection, and signed inbound
provider webhooks remain the next E1.9 slices.

**Amendment (2026-08-28, product configuration):** Alert routing metadata is
now UI/API configurable through `/api/v1/alerting/config` and the `/alerting`
console page. The config is stored in `app_settings`; raw webhook URLs are
rejected so provider secrets remain Kubernetes Secret-backed environment
variables. `/api/v1/alerting/test` exercises the active policy/contact-point
path with a synthetic notification.

## ADR-045: Ownership UI is read-only for installation-scoped deployments

**Status:** Accepted (2026-08-27)

**Context:** The initial E1.2 slice exposed a Teams page where an administrator
could create teams, edit contacts, and assign workloads. That assumes one
Consize instance is a shared multi-team control plane. The intended deployment
model is one team per installation, with collector scope and on-call routing
defined at install time.

**Decision:** Remove team creation, contact-editing, and manual assignment
controls from the UI. Keep the ownership route as a read-only installation
view while the deployment configuration and E1.9 incident-routing work land.
The existing API/store compatibility remains temporarily so current data is
not discarded; new installation onboarding will replace it with deployment
metadata and provider references.

**Consequences:** The UI no longer suggests that operators should maintain a
second team directory inside Consize. Ownership context remains visible, but
changes are made through installation configuration and the organization's
incident-management system. The revised route uses tighter, flatter console
surfaces and a C/S monogram lockup rather than the previous generic diamond
mark.

**Follow-up correction (2026-08-27):** the interim ownership page was judged
unnecessary and was removed from product navigation; `/teams` redirects to the
dashboard and workload detail has no manual ownership controls. The C/S
monogram and split `conSize` wordmark are the active brand treatment.

## ADR-046: Weekly reports use the alerting route and on-demand PDF export

**Status:** Accepted (2026-08-28)

**Context:** M4 left the weekly digest open after the dashboard rewrite. The
MVP now has working Slack contact points, verified-apply savings, and a live
audit trail, so weekly reporting can be useful without adding PagerDuty/JSM
incident ownership yet.

**Decision:** Add a report builder over the existing store contract rather
than a second reporting database. A report covers 7, 14, or 30 days and
includes realized monthly savings from passed verifications in that period,
current pending monthly opportunity, top pending recommendations, rollbacks,
verification failures/inconclusive runs, and telemetry freshness. Admins can
enable the scheduled weekly digest from the `/reports` page; the CronJob runs
weekly and exits cleanly while disabled. Manual generation is always available
through JSON and a simple PDF export. Slack delivery reuses the Alerting
contact-point/policy path so webhook secrets remain Kubernetes Secret-backed.

**Consequences:** Reporting is an operations pulse, not a competing source of
truth. The first implementation is enough for stakeholder visibility and demo
proof: generate report, download PDF, send to Slack, and toggle weekly
delivery. Email delivery, richer PDF layout, attached PDFs in Slack, and
month-over-month trend charts remain future reporting polish.

## ADR-047: Cloud-waste scans create IaC change plans first

**Status:** Accepted (2026-08-28)

**Context:** The next feature list calls for Consize to find non-workload
resources that still accrue cost — unattached storage volumes, idle load
balancers, unused NAT gateways, and stopped instances — and to avoid
configuration drift by changing source-controlled infrastructure where possible.

**Decision:** Add a broader `costscan.Source` seam and persist findings as
cost opportunities. The live GKE deployment uses a GCP source backed by the
Compute Engine API and the existing `consize-gcp` service-account key. The
fixture source remains only for tests and explicit local demos. The first live GCP
implementation emits high-signal findings for detached Persistent Disks and
stopped VMs whose Persistent Disks still accrue cost; idle load balancer and
unused Cloud NAT findings require traffic evidence and remain the next
monitoring-backed scan extensions. Each opportunity records provider, account,
region, resource identity, estimated monthly cost, evidence, risk, recommended
action, and optional Terraform metadata. Operators can prepare an audited IaC
PR, and the GitHub integration can create the branch/commit/draft PR when
credentials are configured. Direct cleanup is deliberately narrower: v1 supports
only GCP unattached Persistent Disks, and revalidates the disk is still detached
immediately before requesting deletion.

**Consequences:** The UI now has a Cloud waste section where admins/operators
can run the scan, review evidence, open an IaC PR, or use Direct cleanup for
supported low-ambiguity resources. Direct cleanup records an insert-only
`cost_actions` trail before and after the provider call, so cleanup does not
vanish when an opportunity is marked resolved. This keeps the safe Consize
pattern intact: observe → recommend → reviewable source change by default;
direct provider mutation only where the provider can be re-checked at execution
time.

## ADR-048: Verification windows scale by apply step

**Status:** Accepted (2026-08-31)

**Context:** The original 24 h verification window was safe but too slow for
the normal rightsizing loop. A first step in Consize is deliberately modest,
because the apply engine already limits Kubernetes changes to 30% per step and
database changes to one adjacent class. Waiting a full day after every small
step makes the product feel stuck, while using a flat short window for every
step would under-observe deeper reductions.

**Decision:** Replace the fixed 24 h shipped default with a 1 h base window
that scales by apply step number. Step 1 uses 1 h, step 2 uses 2 h, step 3 uses
3 h, and so on. The verifier uses the effective step window for both the
pre-apply baseline and the post-apply observation period, so comparisons remain
balanced. The dashboard status endpoint computes the same due time from the
insert-only apply event's stored `step_number`.

**Consequences:** First-step feedback is much faster, follow-up steps still get
progressively more evidence, and no namespace/workload labeling policy is
needed for the MVP. `CONSIZE_VERIFY_WINDOW` remains configurable, but it now
means the base window, not a flat window for every apply. Installations that
need a more conservative posture can raise the base duration.
