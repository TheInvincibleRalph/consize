# Consize — Architecture

## 1. Goals & non-goals

**Goals**
1. Answer "what's the waste?" for compute (k8s) and data (DBs) with a single platform.
2. Remove waste **safely**: every change is guarded, verified, and reversible.
3. Prove the savings: projected vs realized, per workload, per team, over time.

**Non-goals (v1)**
- No autoscaling *policy management* (HPA tuning) — Consize adjusts static requests/limits and instance sizes only.
- No right-sizing of storage volumes, queues, or serverless memory (designed as future surfaces).
- No multi-cluster orchestration — one cluster per deployment, designed so a fleet collector can come later.
- No billing/FinOps platform — Consize consumes pricing data; it does not ingest bills.

## 2. System context

```
┌──────────────┐   ┌──────────────────┐   ┌────────────────┐
│  Operators    │   │  Developers      │   │  On-call       │
│  (dashboard)  │   │  (API/UI)       │   │  (alerts)      │
└──────┬───────┘   └────────┬─────────┘   └───────┬────────┘
       │                    │                     │
       └───────────────┬────┴─────────────────────┘
                       ▼
                 ┌──────────┐
                 │  Consize   │
                 └────┬─────┘
       ┌──────────────┼──────────────┐
       ▼              ▼              ▼
┌───────────┐  ┌─────────────┐  ┌──────────┐
│  Cluster  │  │  Cloud DBs  │  │  Pricing │
│ (k8s API, │  │ (RDS, GCP,  │  │  catalogs│
│ Prometheus)│  │  CloudSQL) │  │  (AWS,   │
└───────────┘  └─────────────┘  └──────────┘
```

## 3. Components

### 3.1 Collector
Pulls and normalizes raw telemetry into the store.

| Source | Data | How |
|---|---|---|
| Prometheus | Per-container CPU (`container_cpu_usage_seconds_total`), memory (`container_memory_working_set_bytes`), throttling, OOMKill events | HTTP API, range queries, 15-min buckets |
| k8s API | Current requests/limits, workload metadata (deployment, namespace, labels, owner) | List/watch |
| Cloud DB metrics | CPUUtilization, FreeableMemory, Read/WriteIOPS, DatabaseConnections, replicas, instance class | Provider SDK |
| Pricing catalogs | On-demand hourly price per request unit / instance class | AWS Price List API, GCP Cloud Billing Catalog (cached locally, refreshed daily) |

Collector is idempotent: buckets are upserted by `(resource, metric, window_start)`. A backfill flag replays history. Runs as a k8s CronJob, hourly.

### 3.2 Analysis Engine
The core logic, pure functions over stored telemetry — no I/O inside (unit-testable).

**Compute surface** — per workload:
1. Take 14 days of 15-min buckets → per-day p95, then aggregate percentiles across the window: `p50, p95, p99, max`.
2. Recommendation policy (default, configurable):
   - `request = ceil(p95 × 1.2)` — covers 95% of real usage with 20% headroom.
   - `limit  = ceil(request × max(2, p99/request))` — burst allowance, never below 2× request.
   - Never recommend above the workload's current values (downsize-only in v1; upsizing is a future toggle).
3. Skip conditions: no data (< 5 days), exclusions (label `consize.savings.dev/exclude=true`), workloads without stable usage, stateful workloads whose `data-loss-risk` flag is set.
4. Savings: `(current_request − recommended) × price_per_unit × 730h/month`, per resource, summed per workload.

**Data surface** — per DB instance:
1. Compute per-day p95 of CPU, max of IOPS, min of FreeableMemory, max connections.
2. Headroom guarantees: candidate instance must satisfy `p95 CPU < 60%`, `IOPS headroom > 40%`, `free memory > 25%`, `connections < 70% of max`.
3. Pick the smallest cheaper instance class satisfying all constraints — **one size step at a time** (no skipping 2+ classes).
4. If no candidate: emit "keep" with rationale (bottleneck attribution: CPU / IOPS / memory / connections).

**Output:** a `Recommendation` record — resource, current, proposed, delta, savings/mo, confidence (based on data volume), risk flags, policy version.

### 3.3 Recommendation Store (Postgres)
Schema highlights:
- `workloads` (type: compute|db, identifiers, namespace, labels, owner_team)
- `usage_buckets` (workload_id, metric, window_start, p50, p95, p99, max)
- `recommendations` (id, workload_id, policy_version, current, proposed, savings_monthly, status: pending|approved|applied|verified|rolled_back|rejected, created_at)
- `apply_events` (id, recommendation_id, actor, mode, result, evidence)
- `verification_runs` (id, apply_event_id, baseline, post, verdict, thresholds)

### 3.4 Apply Engine
Safety layer between recommendation and cluster.

**Guardrails (all evaluated before any change):**
1. Dry-run by default; apply requires `mode: auto` (namespace policy) or explicit approval.
2. Auto-apply only in namespaces labeled `consize.savings.dev/auto-apply=enabled`; everything else = manual approval.
3. Exclusions win: `consize.savings.dev/exclude=true`, stateful workloads with `data-loss-risk=true`, workloads in protected namespaces (`kube-system`, `consize-system`).
4. Step limit: never change a resource by more than 30% per apply (repeated applications step down).
5. No concurrent applies to the same namespace; global concurrency limit.
6. Every apply records `actor` (user or system), `mode`, `dry_run_result`, and a full diff.

**Compute apply:** patch `Deployment.spec.template.spec.containers[].resources` (all replica sets rolled via a normal rollout), or `DeploymentConfig`/`StatefulSet` where enabled.

**DB apply:** only within the instance's maintenance window, one class step, requires `approval` unless policy `auto-db` is explicitly enabled. Never applies to the primary if replicas are absent (failover safety).

### 3.5 Verifier
After each apply, compares SLIs over a window against a pre-apply baseline.

| Signal | Regression threshold |
|---|---|
| Error rate (HTTP 5xx / DB errors) | +50% vs baseline, sustained 5 min |
| p99 latency | +30% vs baseline, sustained 5 min |
| CPU throttling events | any sustained increase |
| OOMKilled / evictions | any new events |
| DB: CPU saturation > 85% | sustained 15 min |

- Baseline: 24 h before apply. Verification: 24 h after apply (configurable).
- Verdict `PASS` → recommendation marked `verified`, realized savings tracked.
- Verdict `FAIL` → **automatic rollback** (restore previous requests/limits or previous instance class), mark `rolled_back`, attach evidence, alert on-call via configured channel.

### 3.6 REST API (Go, `net/http` + chi)
| Endpoint | Purpose |
|---|---|
| `GET /api/v1/workloads` | list workloads + summary |
| `GET /api/v1/workloads/{id}/recommendations` | history |
| `POST /api/v1/recommendations/{id}/apply` | trigger apply (mode: dry_run \| approved \| auto) |
| `GET /api/v1/applies` | audit trail |
| `GET /api/v1/savings` | projected vs realized, by team, by time |
| `GET /api/v1/healthz` | liveness |
| `GET /api/v1/readyz` | readiness (store, k8s API reachable) |

### 3.7 UI (React + Vite + Tailwind)
- **Savings overview** — projected vs realized, monthly trend, per team.
- **Recommendations** — ranked by savings, with rationale, risk flags, and one-click apply (respects guardrails).
- **Workload detail** — usage percentiles chart (14 days), current vs proposed, apply history.
- **Apply audit** — every event, who/what/when, verdict, evidence links.
- **Settings** — policies (headroom %, step limits, verification windows), namespace auto-apply labels.

### 3.8 Scheduler
k8s CronJobs: `collector` (usage intake), `costscan` (cloud-waste opportunities), `analyze` (recommendations), `verify` (pending applies), `report` (weekly savings digest).

## 4. Data flow

**Read path (analysis):**
```
Prometheus/Cloud → Collector → usage_buckets → Analysis Engine → recommendations → API → UI
```

**Write path (apply):**
```
UI/API → guardrails → Apply Engine → k8s patch / DB modify
       → Verifier (baseline+post SLIs) → PASS → verified
                                       → FAIL → rollback → alert
```

## 5. Runtime topology

Consize runs on the cluster it manages (self-hosted):

- `engine` — Deployment (API + scheduler) or separate Deployments per binary (`engine-api`, `engine-worker` in v1 = single Deployment, split later).
- `collector`/`costscan`/`analyze`/`verify` — CronJobs.
- `ui` — Deployment behind an Ingress, OIDC-protected.
- Postgres — managed instance (RDS/GCP SQL) *outside* the managed cluster (never right-size your own store).
- Prometheus — existing cluster stack; Consize consumes it.

## 6. Interfaces & contracts

- **k8s access:** read-only ServiceAccount for collector/analysis; write ServiceAccount for apply, RBAC-scoped to namespaces with auto-apply (see `docs/security.md`).
- **Cloud DB access:** read-only for metrics; `rds:ModifyDBInstance`-style write permission for apply, restricted by resource ARN.
- **Pricing cache:** daily refresh; stale data never silently used — cache age is part of confidence scoring.
- **Verification signals:** consumed from Prometheus via recording rules shipped with the Helm chart.

## 7. Failure modes & resilience

| Failure | Behavior |
|---|---|
| Prometheus unreachable | Collector retries with backoff; analysis skips workloads with stale data (confidence drops) |
| k8s API transient failure | Apply retries idempotently (patch with `resourceVersion` guard); never double-applies |
| DB modify fails mid-apply | Provider-atomic; verifier still runs; if partial, rollback restores previous class |
| Verifier data missing | Waits up to 2 h; verdict = `inconclusive`, recommendation not marked verified |
| Store down | API returns 503 via readyz; apply blocked (never apply without audit trail) |
| Clock skew | All bucketing uses `window_start` from collector, not `now()` |

## 8. Configuration surface

Everything is YAML-configurable: percentile targets, headroom %, step limits, verification windows and thresholds, namespace policies, exclusion patterns, team label key. Config lives in a ConfigMap, validated at startup, versioned in the Helm chart.

## 9. Roadmap surfaces (post-v1)

Storage throughput, queues, serverless memory — same engine, new adapters. Fleet mode (many clusters → one Consize). Upsizing recommendations behind an explicit toggle.
