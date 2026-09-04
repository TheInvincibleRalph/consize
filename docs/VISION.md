# Consize — Vision & Roadmap

> **Rightsize your database and Kubernetes workloads with a safety engine.**
> Analyze → Recommend → Apply → Verify → Persist/Rollback.

---

## The Problem

Cloud infrastructure teams waste 30–50% of their compute and database spend not because they lack visibility — but because they lack *confidence*.

They can see the Grafana dashboards showing a workload using 12% of its requested CPU. They can see that RDS instance with average utilization under 20%. But making the change is terrifying. What if it OOMKills in the middle of the night? What if the database becomes the bottleneck during a traffic spike after a resize? What if the change causes a production incident and there is no automated way to revert?

So the spreadsheet lives on, the Jira ticket ages, and the cloud bill grows.

**Consize exists to close that gap.** Not just another cost visibility dashboard — a fully automated safety engine that analyzes, applies, verifies, and rolls back infrastructure changes with complete auditability.

---

## The Vision

**Consize's goal is to be the standard open-source platform for safe infrastructure optimization — the way ArgoCD is the standard for GitOps, and Prometheus is the standard for metrics.**

We believe that:

- **Math, not magic:** Every recommendation must be explainable and reproducible. Infrastructure changes are too consequential for black-box AI guesswork. Consize uses deterministic statistical models (p95/p99 over 14-day windows) that any engineer can audit and understand.
- **Safety is the product:** Identifying savings is the easy part. The hard part is applying those changes without breaking production. The verifier, auto-rollback, and SLI safety gates are not features — they *are* Consize.
- **GitOps native:** Optimization should never cause configuration drift. The source of truth for infrastructure should remain your Git repository. Consize integrates with your IaC workflows rather than fighting them.
- **Least privilege by design:** Consize requests only the permissions it needs, scoped to exactly the namespaces you allow. It never applies changes silently. Every action is logged, attributed, and reversible.
- **Open and self-hosted first:** We believe in the [Grafana model](https://grafana.com/blog/2019/10/03/how-open-source-grew-to-become-the-leader-in-monitoring/). The core engine is, and always will be, open source and self-hostable. Organizations should be able to run Consize completely inside their own network without sending workload data to any third party.

---

## How it Works

Consize follows a strict four-phase safety loop for every change it makes:

```
ANALYZE ──► RECOMMEND ──► APPLY (step-wise) ──► VERIFY ──► ✅ Done
                                                      └──► ❌ AUTO-ROLLBACK ──► ALERT
```

1. **Analyze:** The Collector ingests Prometheus metrics (CPU/memory usage, database SLIs) on a configurable cadence and computes 14-day p50/p95/p99/max percentile windows.
2. **Recommend:** The Analysis engine generates statistically-grounded recommendations. CPU requests are set to `p95 × 1.2`. Limits are set to `max(2×request, p99)`. Recommendations with less than 5 days of data are withheld. Skip conditions (excluded labels, data-loss-risk workloads, already-optimal workloads) are applied.
3. **Apply:** Changes are applied in steps, never all at once. A 50% reduction is applied as two separate 25% steps. Each step uses `resourceVersion`-guarded Kubernetes patches, preventing race conditions with other controllers.
4. **Verify:** After each apply step, the Verifier monitors SLIs (CPU throttling, OOMKill count, restart rate, application-level error rate, p99 latency) for a configurable window that scales with the step number. If any SLI exceeds its threshold, the system immediately reverts to the byte-identical pre-apply values.

---

## Try the Interactive Sandbox

*"Show, don't tell."*

We packaged the entire Consize backend (API, Verifier, Prometheus Stub, Postgres) and the Next.js UI into a single, self-contained interactive demo container. 

You don't need a Kubernetes cluster or any external dependencies to run this. Just run:

```bash
docker run -p 3000:3000 ghcr.io/theinvincibleralph/consize-sandbox:latest
```

Then open [http://localhost:3000](http://localhost:3000) in your browser. 

The sandbox comes pre-seeded with historical data and a simulated workload (`checkout-api`). When you apply the recommendation for `checkout-api`, a background injector will intentionally spike OOMKill metrics, allowing you to watch the Consize Verifier catch the breach and instantly auto-rollback the change to restore safety.

---

## Current State

Consize is **production-ready** and deployed on live GKE clusters. The core safety engine is fully implemented, tested, and battle-tested with real workloads including deliberate regression scenarios and live rollback verification.

| Surface | Status |
|---|---|
| Kubernetes compute rightsizing (CPU & Memory) | ✅ Production |
| GCP Cloud SQL & AWS RDS rightsizing | ✅ Production |
| Step-wise apply with SLI guardrails | ✅ Production |
| Automatic rollback with evidence | ✅ Production |
| Full audit trail (actor, diff, verdict) | ✅ Production |
| Multi-user auth (Viewer / Operator / Admin) | ✅ Production |
| GCP Cloud Waste scanning (detached disks, stopped VMs) | ✅ Production |
| GitHub IaC PR workflow (Terraform/YAML) | ✅ Production |
| Alerting with Slack Block Kit & routing policies | ✅ Production |
| Weekly savings digest reports | ✅ Production |
| Helm chart for production install | ✅ Production |
| Next.js dashboard (dark/light, command palette) | ✅ Production |

---

## Roadmap

The Grafana project defined its roadmap by this principle: *"We build with the community, not for the community."* This roadmap is a directional guide, not a contract. Community feedback, pull requests, and real-world adoption will shape its evolution.

### Phase 1 — Ecosystem Expansion
*Broaden the surfaces Consize can scan and optimize.*

- [ ] **AWS Waste Scanning:** EBS unattached volumes, stopped EC2 instances, idle Elastic Load Balancers, unused Elastic IPs.
- [ ] **Azure Support:** AKS compute rightsizing and Azure Monitor database metrics.
- [ ] **Idle Load Balancer & NAT Gateway Detection:** Traffic-backed checks for cost elimination without service disruption.
- [ ] **GPU Rightsizing:** Percentile-based GPU utilization analysis using DCGM/NVML metrics.

### Phase 2 — Policy & Governance
*Give teams control over the "when" and "who" of optimization.*

- [ ] **Snooze & Exempt Policies:** Audit-backed workflows to snooze a recommendation or permanently exempt a workload, with required reasons and configurable expiry.
- [ ] **Policy-as-Code:** Define guardrails declaratively (e.g., `max_step_reduction: 20%`, `require_approval: true`, `protected_namespaces: [payments]`) so platform teams can set guardrails without code changes.
- [ ] **Cost Budget Alerts:** Namespace and team-level spend budgets with proactive alerts when projections exceed thresholds.

### Phase 3 — Incident Integration & Observability
*Close the loop between Consize and your incident management workflows.*

- [ ] **PagerDuty Integration:** Two-way sync: Consize opens an incident on rollback; acknowledgement and resolution flow back to the dashboard.
- [ ] **Jira Service Management Integration:** Ticket creation, assignment sync, and resolution on verification PASS.
- [ ] **Self-Observability (System Status):** A `/api/v1/system/status` contract and pipeline status card so operators can immediately see if telemetry is stale or the collector loop is degraded.
- [ ] **Grafana Dashboards:** Pre-built Grafana dashboard definitions for Consize's own engine metrics (applies, verifications, savings rate, rollback rate).

### Phase 4 — GitOps & IaC Depth
*Make optimization a first-class citizen of your CI/CD pipeline.*

- [ ] **GitLab & Bitbucket Support:** Parity with the existing GitHub PR workflow.
- [ ] **Repository Ownership Discovery:** Automatically route PRs and Slack alerts to the team that owns the workload based on `CODEOWNERS` or label-to-team mappings.
- [ ] **Flux CD Integration:** Native support for the Flux GitOps toolkit alongside ArgoCD patterns.
- [ ] **Helm Value Patching:** Identify and patch `resources:` blocks in Helm `values.yaml` files, not just raw Kubernetes YAML.

### Phase 5 — The Agent Model (Fleet Scale)
*Scale from one cluster to the entire organization without operational overhead.*

The current architecture is an excellent single-cluster, self-hosted tool. This phase introduces a lightweight agent to enable multi-cluster fleet management — following the pattern proven by Grafana Agent, Prometheus Remote Write, and ArgoCD ApplicationSets.

- [ ] **Consize Agent:** A lightweight, read-only agent that runs inside any cluster (GKE, EKS, AKS, bare metal) and pushes telemetry outbound over HTTPS. No inbound firewall rules. No database to manage.
- [ ] **Managed Control Plane (Consize Cloud):** A centrally-hosted, multi-tenant API and dashboard where all agents report. One login, one global view of cloud waste across all clusters.
- [ ] **Single Pane of Glass:** Global savings, cross-cluster apply history, and fleet-wide policy management in one dashboard.

---

## Community & Governance

Consize follows the **open-core** model:

- **OSS Core (This repo):** The engine (collector, analyzer, verifier, rollback), the API, the Next.js UI, and the Helm chart are open source and free forever.
- **Consize Cloud (Coming soon):** The managed multi-tenant control plane for teams who don't want to operate the stack themselves.

We are aiming to follow governance patterns established by CNCF projects, with a public decision log (our ADR records in `docs/decisions.md`) and a contributor-friendly development environment.

### How to Contribute

1. **Spin up the local environment in under 5 minutes:**
   ```sh
   # Requires: Docker, kind, Go 1.22+
   git clone https://github.com/YOUR_ORG/consize.git
   cd consize
   docker compose up -d   # Postgres + Prometheus with synthetic fixtures
   go run ./engine/cmd/api
   cd ui && npm install && npm run dev
   ```

2. **Pick an issue** — look for labels:
   - `good first issue` — Well-scoped, great for first-time contributors.
   - `help wanted` — Higher complexity, maintainer guidance available.
   - `cloud-provider` — Adding a new cloud scanner or metrics adapter.

3. **Propose a change** — Open a GitHub Discussion before large PRs. We use Architecture Decision Records (ADRs) in `docs/decisions.md` to record significant decisions. New contributors are encouraged to write ADRs for features they add.

4. **Join the conversation:** GitHub Discussions, or reach out directly on [Twitter/X](https://twitter.com/) for design feedback.

---

## Design Decisions & ADR Log

Every significant technical decision in Consize is recorded as an Architecture Decision Record in [`docs/decisions.md`](docs/decisions.md). This includes decisions like:

- Why we chose p95×1.2 for CPU requests (ADR-005)
- How we handle the baseline-unavailability problem (ADR-027)
- Why we separated the reader and writer ServiceAccounts (security model)
- Why the embedded SPA stays in the API binary (single-binary fallback)

Reading the ADR log is the fastest way to understand *why* Consize is designed the way it is.

---

## Positioning

| Tool | What it does | What Consize does differently |
|---|---|---|
| **Kubecost / OpenCost** | Real-time cost visibility & allocation | Consize *acts* on waste — it applies, verifies, and rolls back changes automatically |
| **CAST AI / StormForge** | ML/AI-driven automated rightsizing | Consize uses deterministic math; every recommendation is auditable and reproducible |
| **VPA (Vertical Pod Autoscaler)** | In-cluster resource auto-scaling | VPA has no safety verification loop, no rollback, and no cross-surface (DB) coverage |
| **Infracost** | Cost estimates in CI/CD pipelines | Infracost shows future cost; Consize acts on existing waste with a safety net |

Consize's unique position is the **Safety Engine** — the verifier, step-wise apply, and auto-rollback that gives platform teams the confidence to actually deploy optimizations, not just report them.

---

*This document is a living roadmap. It is updated as the project evolves. Significant changes to project direction will be noted in [docs/decisions.md](docs/decisions.md).*

