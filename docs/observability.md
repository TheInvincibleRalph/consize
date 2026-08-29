# Consize — Observability & Reliability

The platform that watches the infrastructure's waste must itself be watched. Consize is instrumented with OpenTelemetry (metrics, logs, traces) and ships with Grafana dashboards + alert rules in its Helm chart.

## 1. Instrumentation

- **Metrics (OpenTelemetry → Prometheus):**
  - `consize_collector_buckets_written_total{source}` — collector throughput
  - `consize_collector_scrape_failures_total{source}` — data pipeline health
  - `consize_analysis_runs_total{outcome}` / `consize_analysis_duration_seconds`
  - `consize_recommendations_created_total{surface}` and `{status}` (pending/applied/verified/rolled_back/rejected) — the product funnel
  - `consize_apply_total{mode, result}` / `consize_apply_duration_seconds`
  - `consize_verifier_verdict_total{verdict}` — PASS / FAIL / INCONCLUSIVE
  - `consize_rollbacks_total{surface}` — the number that must stay near zero
  - `consize_savings_projected_monthly` / `consize_savings_realized_monthly` — the business number, as a gauge
- **Logs:** structured JSON (component, operation, workload, recommendation_id); request-scoped `trace_id` on all API logs.
- **Traces:** OTel spans on collector → analysis → apply → verify path; ingestion → storage → outbound calls.

## 2. Grafana dashboards (shipped as JSON)

**Consize Overview**
- Projected vs realized savings (area chart, monthly)
- Recommendations funnel (created → applied → verified → rolled_back)
- Rollback count & rate (the safety signal)
- Analysis run duration + failures

**Data Pipeline**
- Scrape failures per source; bucket write rate; staleness (max bucket age per workload)
- Backfill progress

**Apply Safety**
- Apply duration percentiles; guardrail-blocked attempts (why: excluded / step-limit / no-approval / concurrency)
- Verification verdicts per surface; INCONCLUSIVE rate (data quality signal)

**Consize Health**
- Pod/CPU/mem; Postgres connections & latency; API p99 latency; readiness failures

## 3. Alerting (alert rules in the Helm chart)

| Alert | Condition | Severity |
|---|---|---|
| ConsizeCollectorStale | max bucket age > 3 h for any source | warning |
| ConsizeAnalysisFailed | analysis cron fails 2× consecutively | warning |
| ConsizeApplyFailure | apply fails after retries | critical (ops can't act safely) |
| ConsizeRollbackTriggered | rollback fired | critical — always page |
| ConsizeVerifierInconclusiveHigh | INCONCLUSIVE rate > 20% over 24 h | warning (data quality) |
| ConsizeSavingsReversal | realized savings decrease > 10% week-over-week | warning |
| ConsizeApiSaturated | API p99 > 500 ms or 5xx rate > 1% | warning |

Routing: `critical` → on-call channel (Slack/email), `warning` → ops channel. Every alert links to the dashboard panel + runbook section.

## 4. Runbooks (documented in the README's troubleshooting guide)

- **"Consize stopped producing recommendations"** — check collector staleness dashboard → Prometheus scrape config → store connectivity.
- **"Apply stuck in pending"** — approval expired (re-approve), or namespace policy changed (config diff).
- **"Rollback fired — what now?"** — follow the evidence link in the alert: verification run ID → baseline vs post SLI charts → the exact diff that was reverted.
- **"Engine can't reach k8s API"** — token expiry (SA projection), RBAC drift, network policy.

## 5. SLOs for Consize itself

| Signal | Target |
|---|---|
| Analysis freshness: every workload analyzed within 24 h of data | 99.9% |
| Verification completeness: verdict produced for 99% of applies within 2 h | 99% |
| Apply success rate (after retries) | 99.5% |
| Rollback correctness: every FAIL produces a rollback | 100% |

These are recorded as SLOs in the Grafana dashboard — dogfooding: Consize's own error budget is visible next to the savings it produces.

## 6. Weekly digest

The scheduler's weekly report includes savings realized from verified applies,
pending recommendations with total value, rollbacks, verification issues, and
telemetry freshness. Admins enable or disable delivery from the Reports page;
the CronJob exits cleanly while disabled. Reports can also be generated on
demand for 7, 14, or 30 days and downloaded as PDF. Slack delivery goes through
the configured Alerting route, so the digest uses the same contact-point
contract as operational alerts.
