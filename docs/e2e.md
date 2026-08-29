# Live-cluster E2E — the M2 safety engine on a real GKE cluster

> Status: **run 2026-08-24 on GKE `devops-portfolio` (project `devops-portfolio-prod`, us-central1)** — see the results section at the bottom.

The M2 acceptance criteria deferred the live-cluster E2E as a CI gate. This runbook closes that gap: the full **analyze → apply (guarded) → verify → rollback** loop exercised against a real cluster, with a least-privilege ServiceAccount doing the patching. The scripts live in [`tests/e2e-live/`](../tests/e2e-live/).

## What it proves

| Track | Scenario | Expected |
|---|---|---|
| 1 | Apply a real recommendation to `boutique/frontend` (real workload), verify after the window | verdict **passed**, no rollback, requests stay applied |
| 2 | Apply a real recommendation to the synthetic canary, then inject a forced regression (memory dropped below its 180 MiB allocator footprint) inside the verification window | verifier **FAILs** on new OOM events → **auto-rollback** restores the applied values, pod healthy again |
| RBAC | `kubectl auth can-i` matrix for the write/read identities | update deployments only in the bound namespaces; nothing else, anywhere |

## Environment (as found 2026-08-24)

- GKE `devops-portfolio` — 7 small nodes (e2-custom-1-4096), 3 zones; free-trial quotas (12 vCPU).
- kube-prometheus-stack in `monitoring` — Prometheus at `monitoring-kube-prometheus-prometheus.monitoring:9090`, kubelet/cadvisor metrics present (the verifier's SLI data).
- CloudNativePG operator in `cnpg-system` (used for the Consize Postgres).
- Namespaces: `boutique` (11d microservices demo, idle), `ai-inference`, `monitoring`, `cnpg-system`, … `test-a`/`test-b` empty.
- **Caveat: the monitoring Prometheus originally had no PVC — fixed mid-run with a 10 Gi `standard-rwo` PVC (ADR-028).** It was reinstalled ~2.4 h before the run, so early history was ephemeral (resets on pod recreation); no workload has 5 days of data → the E2E runs analysis with `CONSIZE_MIN_DATA_DAYS=0.1` (ADR-024). The caveat *actually bit twice* mid-run: the metric history was lost at ~16:40Z (pod recreated — a due verification returned inconclusive; ADR-027) and again at ~21:26Z (this time root-caused: the `application-pool` node autoscaler replaced nodes under the monitoring pods, and the PVC-less WAL died with each — see Results, run 4). The stack now survives node churn: the Prometheus CR carries a volumeClaimTemplate, so history persists across pod/node replacements.
- Artifact Registry: `us-central1` repo creation is rejected on this project ("Requested entity was not found"); the **multi-region `us` location works** → registry `us-docker.pkg.dev/devops-portfolio-prod/consize`.

## Topology

```
boutique (approved-only, no auto-apply label)      consize-sandbox (auto-apply=enabled)
└── frontend — Track 1 target                      └── consize-canary — Track 2 target
                                                       (python, holds 180 MiB, /healthz)

consize-system
├── Postgres (CNPG Cluster consize-db, 1 GiB)   ← the shared audit trail (ADR-008)
├── consize-api      Deployment  (SA consize-writer)  ← the only write surface (ADR-021)
├── consize-collector CronJob   (SA consize-reader)   ← usage_buckets, read-only
├── consize-analyze  CronJob    (store only)          ← CONSIZE_MIN_DATA_DAYS=0.1
├── consize-verify   CronJob    (SA consize-writer)   ← window 15m, sustained 3m (E2E-shortened)
└── consize-store    Secret     ← DATABASE_URL + PROMETHEUS_URL
```

Identities (deploy/rbac.yaml + tests/e2e-live/namespace.yaml):

- `consize-writer` — `update deployments` ONLY, bound to consize-sandbox + boutique (write). Used by api (apply) and verify (rollback) — one write surface.
- `consize-reader` — read-only on deployments/statefulsets/replicasets/pods, bound to the same two namespaces (collector).

## Running it

Prereqs: gcloud + kubectl authenticated to the cluster, docker, jq.

```sh
cd tests/e2e-live

./run.sh preflight   # cluster reachable + kubelet metrics queryable
./run.sh deploy      # images → Artifact Registry, stack, Postgres, migrations, canary
./run.sh ingest      # manual collector + analyze runs → pending recommendations
./run.sh track1      # dry-run → approved apply on boutique/frontend
./run.sh verify1     # waits for the 15m window, verifies, asserts PASSED
./run.sh track2      # approved apply on the canary + inject the OOM regression
./run.sh verify2     # waits for the 15m window, verifies, asserts FAIL + auto-rollback
./run.sh rbac        # auth can-i matrix
./run.sh status      # store + cluster state any time
./run.sh summary     # evidence recap
```

De-risking choices:

- **Two tracks, two verdicts.** Track 1 is the happy path on a *real* application; Track 2 manufactures the rollback on a disposable workload. The regression is injected **after** a real, analysis-driven apply — the policy never recommends below actual usage (request = p95 × 1.2), so analysis itself cannot produce an OOM; the verifier exists precisely to catch regressions that appear during the window, whatever their source.
- **The canary is only applied against after it has real data** (~1.5 h of Prometheus history), so Track 2's recommendation is genuine, not seeded.
- **Images are rebuilt and pushed each deploy**; the deploy script is idempotent (repo/secret/manifests are all re-appliable).
- **API access is a local port-forward** (ClusterIP only — nothing exposed to the internet).
- **Shortened windows**: the deployed verify CronJob uses `CONSIZE_VERIFY_WINDOW=15m`, `CONSIZE_SUSTAINED_MINUTES=3` for a session-length run; the shipped defaults are 24 h / 5 m (ADR-018/019).
- **Database surface (M3, ADR-033)**: the collector CronJob sets `CONSIZE_DBMETRICS=fixture`, so the deployed stack also ingests the deterministic demo instance `rds/payments-prod` (`db.t3.large` → `db.t3.medium`, $50/mo, confidence 100%, verified locally end-to-end against Postgres on 2026-08-25). Apply it the same way as any recommendation — `mode=approved` with an actor inside its maintenance window (`sun:00:00-sat:00:00` UTC, i.e. any moment except Saturday UTC); a real apply returns the stub's "manual class change required" error until a live provider lands, and dry-runs work end to end. Its `auto-db=enabled` label demonstrates the approval-default guardrail's auto path.

## Teardown

```sh
./run.sh status                        # eyeball state first
tests/e2e-live/teardown.sh             # remove sandbox + system namespaces, images
tests/e2e-live/teardown.sh --restore-boutique   # also restore frontend's pre-E2E requests
```

---

## Results — run of 2026-08-24 (GKE devops-portfolio)

Evidence files in `tests/e2e-live/out/`; the full audit trail (apply events, verification runs) is in the Postgres store, visible through the API (`./run.sh status`).

### Track 1 — PASS on a real application (boutique/frontend) ✅

The pipeline ran end-to-end against the demo store's frontend:

| Step | Evidence |
|---|---|
| Recommendation (memory) | `64 MiB → 44.8 MiB` (46 976 205 B), savings $-tracked, confidence scored |
| Guardrail dry-run | step plan 1/6 (30 % steps, downsize-only, QoS-preserving) |
| Approved apply (`actor=e2e`) | apply event #4, `applied`, 14:55:04Z |
| Verification (window 15 m) | run #1 verdict **passed** — restarts 0→0, OOM 0→0, evictions 0→0 (throttling `unavailable`: this kube-prometheus-stack version does not export `container_cpu_cfs_throttled_seconds_total` — a data gap, not a failure) |
| Aftermath | no reverted event; frontend still applied at 44.8 MiB/89.6 MiB (`track1-resources.json`) |

The step follow-up (#12) was queued as designed — the first step landed, the rest wait for verification.

### Track 2 — FAIL → auto-rollback (consize-canary, synthetic) — five attempts, three lessons

The canary received a genuine analysis-driven apply (256 MiB → ~233 MiB, memory), then an externally injected regression (dropped to 64 MiB/96 MiB → OOMKill → CrashLoopBackOff) during the verification window. It took five attempts because each attempt surfaced something real: a patcher bug, two distinct monitoring data-loss failures, and a scheduling race.

- **Run 1 — FAIL and rollback *worked*, and exposed a real bug.** Verdict **failed** at 15:29Z (restarts 0 → 36.13 post vs baseline), alert logged, `reverted` event #6 recorded, canary restarted… but on **87 MiB/142 MiB** instead of the pre-apply 256 MiB/512 MiB (`track2-restored.json`). The rollback had inverted the apply diff onto the *drifted live state* — exactly the drift case the verifier exists to catch. Root-caused and fixed: `Rollback` now reads live totals and patches an honest `live → pre-apply` diff (ADR-026), unit-tested (`TestRollbackAfterDrift`). **The E2E found a defect no unit test had.**
- **Run 2 — infrastructure data loss, conservatism held.** The retry apply (#7, 15:37Z) was verified by the scheduled CronJob at 16:07Z, but the ephemeral Prometheus had been recreated (~16:40Z) and the pre-apply baseline was gone: every SLI returned `inconclusive` ("data missing in one window"). **No rollback fired** — without baseline evidence Consize refuses to act (ADR-027). Inconclusive is recorded and terminal; the manual re-verify attempt (16:53Z) then hit a packaging error (an arm64 verify image — `exec format error`), fixed and re-pushed (amd64, digest `d9d6ddb`).
- **Run 3 — FAIL → rollback proven live, and the assertion suite caught its own test bug.** Fresh apply (#8, 20:15Z) → injection → verdict **failed** at ~20:32Z (OOM events 0→N in the post window) → auto-rollback → canary **restored to exactly the pre-apply memory values** — the ADR-026 absolute-restore semantics proven on a live drifted workload. The restore *assertion* then failed on the CPU fields: the injection had also lowered CPU (100m/500m), and Consize rightly does not undo an external actor's unrelated change. The test, not the engine, was wrong: the injection is now memory-only, matching the apply/rollback contract.
- **Run 4 — the scheduled verifier raced the manual one; then the cluster's node autoscaler ate the metrics.** Fresh apply (#10, 20:39Z) → memory-only injection → canary crashed from 21:04Z (after a scheduling fix: the 1000m canary could not land on the 1-vCPU node while the old pod held it — scale 0→1). At 21:07:02Z the **scheduled CronJob** fired seconds before the manual verify2 and recorded a *premature `passed`* (its post window hadn't accumulated the crash data yet — post restarts 0 while `kubectl` showed the count climbing). The wrong row was deleted (a recording error under the ADR-023 upsert), evidence soaked to 9 restarts — and then the probe came back null: the **application-pool node autoscaler had replaced nodes at 20:48Z/21:12Z/21:26Z**, and the PVC-less monitoring Prometheus lost its whole WAL with the last one. Event #10 became unverifiable (baseline gone) and was superseded.
- **Run 5 — the headline run, on durable SLI storage.** Root cause of runs 2 and 4 fixed: the monitoring Prometheus now has a 10 Gi `standard-rwo` PVC (ADR-028), the scheduled verify CronJob was suspended for the remainder of the session (the run book below is manual anyway), the canary was restored to 256 MiB/512 MiB, and its poisoned usage buckets were cleaned. Fresh recommendation → apply → injection → verification → FAIL → auto-rollback → assertions.

Run 5 ran clean, start to finish, on the durable stack — apply #11 → verdict **failed** → auto-rollback → assertions, all green:

| Step | Evidence |
|---|---|
| Recommendation | #384: memory 256 MiB → 234 MiB (245 366 784 B), savings \$0.08/mo, confidence 7 % (analysis-driven from 5 clean usage buckets, p95 ≈ 194 MiB × 1.2) |
| Approved apply (actor=e2e-bot) | event #11, `applied`, 23:01:30Z — deployed 234 MiB/468 MiB (QoS-preserving ratio), write SA |
| Regression injected (bad release) | memory dropped to 64 MiB/96 MiB → OOMKill → CrashLoopBackOff, restarts 3+ before the window closed (scheduling deadlock fixed ahead of the run: canary CPU request 1000m → 100m on the 1-vCPU nodes) |
| Verification (window 15 m) | run #7 verdict **failed** 23:17:43Z — **restarts 0 → 32.80** post vs baseline (the firing SLI; OOM/evictions flat, throttling `unavailable` — this stack exports neither counter), alert logged `consize: FAIL verification … rolling back` |
| Auto-rollback | `reverted` event #12, 23:17:44Z; `track2-restored.json` **identical** to `track2-orig.json` (`diff` clean) — pre-apply 256 MiB/512 MiB restored absolutely (ADR-026) |
| Aftermath | canary 1/1 Running, 0 restarts, 180 MiB allocator fits the restored request |

The run that took five attempts delivered the headline evidence: **analyze → apply → injected regression → FAIL verdict with SLI evidence → alert → auto-rollback → byte-identical restore → healthy workload**, all through the least-privilege ServiceAccount, with the full trail in the Postgres store (`status` shows events #4–#12: 1 passed, 3 failed, 2 inconclusive, 3 reverted).
