# Consize — The Demo Script

**The 10-minute story:** *"Here is a cluster with real waste. Watch Consize find it, fix it, prove the savings — and then prove it can take it all back if something breaks."*

This is the script for the recorded demo video and the README's cover story. Every number in it is produced live by the running product — no screenshots of mockups.

---

## 0. Setup (before the camera — 20 minutes)

- Sandbox GKE cluster with Consize deployed (Terraform + Helm, all from `make deploy`).
- 10 fixture workloads with inflated requests (e.g., `requests: 8Gi / usage: 300Mi`), 2 with realistic usage, 1 excluded (`consize.savings.dev/exclude=true`), 1 in a protected namespace.
- One oversized RDS instance (e.g., `db.r6g.2xlarge` running at ~9% CPU) with the maintenance window set to *tonight*.
- Slack/alert channel wired to the Consize alerts.

## 1. The pain (30 seconds)

Show the cluster: `kubectl get pods` with `requests: 8Gi` — and the Prometheus graph showing 300 MiB of actual usage. One sentence: *"This is normal. This waste exists in every cluster. Nobody fixes it because fixing it manually is a spreadsheet job — and risky."*

## 2. Consize finds it (90 seconds)

Open the dashboard → Recommendations, sorted by savings.

- 10 compute workloads with recommendations, each with: current vs proposed, usage chart (14-day percentiles), rationale ("p95 = 1.1 Gi; request 1.4 Gi covers 95% of real usage with 20% headroom"), savings per month.
- The DB instance: one-class-down recommendation with headroom guarantees shown.
- The excluded and protected workloads: present in the list with `skipped — policy` and the reason — *"Consize doesn't touch what you tell it not to."*
- Total: **$2,340/month projected**.

## 3. Consize fixes it (3 minutes)

- Click **Apply (dry-run)** on one workload → show the exact patch diff. *"Nothing changed — this is the preview."*
- Click **Apply** → watch the rollout in the workload history.
- Run the live verification: show the dashboard flipping to `verified` after the window with baseline vs post-SLI charts flat.
- Apply the rest (namespaces are auto-apply labeled; Consize steps each down in ≤30% increments).
- DB: attempt apply → *"blocked — outside maintenance window"* → approve with the window → verify.

## 4. Consize proves it (60 seconds)

Savings overview: **realized $2,260/month** — with the per-workload evidence trail and the team attribution. *"Projected is a number; realized is an audit trail."* Show the apply audit table: every change, who, when, diff, verdict.

## 5. Consize takes it back (2 minutes — the money shot)

Deploy a latency bug to one workload (pre-staged), then apply its rightsizing. Watch:

1. Verifier detects error rate +50% / p99 +30%.
2. **Automatic rollback** fires — pod requests/limits restored to previous values.
3. Alert hits Slack: "rollback triggered, evidence link".
4. Recommendation marked `rolled_back` with the diff and the SLI charts attached.

One sentence: *"Consize only saves money when it's safe. When it isn't, it's the fastest thing in the building at saying 'I was wrong'."*

## 6. Close (30 seconds)

- Dashboard summary shot: savings trend, zero rollbacks (except the one we just did), data quality green.
- The pitch line: *"Find the waste. Remove it safely. Prove it."*
- Link in the description: repo, docs, architecture diagram.

---

## Recording notes

- Screen at 4K; terminal font large; every step reads from the live UI (no edits).
- The rollback section is rehearsed 3× before recording — it's the part people remember.
- Subtitle the demo: the video is also the README for people who won't read.

## Script variants

- **15-second version:** recommendations → apply → verified → savings number.
- **5-minute version:** drop section 1 (pain) and section 6 (close) to a sentence each.
