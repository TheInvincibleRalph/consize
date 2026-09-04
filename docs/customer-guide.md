# Consize

Consize helps engineering teams reduce infrastructure waste without turning cost optimization into a production risk.

It analyzes real usage from Kubernetes workloads and cloud database instances, recommends safer sizes, and gives teams two ways to act: open a reviewable Infrastructure-as-Code pull request, or apply a guarded runtime change directly. Every change is recorded, verified, and reversible.

> Image placeholder: product dashboard showing savings, open recommendations, system health, and recent verification results.

## Why teams use Consize

Most infrastructure waste is obvious only after it becomes expensive. A service requests 8 GiB and uses 400 MiB. A database runs one class too large for months. A detached disk keeps accruing cost after an environment is deleted. Engineers usually know these problems exist, but they hesitate to change production resources without evidence, review, and rollback.

Consize closes that execution gap.

It is not just a scanner that says “you could save money.” It is a safety system for acting on that information:

```text
collect telemetry → analyze usage → recommend changes → apply safely or open an IaC PR
                 → verify health → rollback on regression → keep an audit trail
```

The result is a product engineers can trust: recommendations are backed by usage data, changes move in controlled steps, risky workloads are blocked, and realized savings are counted only after verification.

## What Consize optimizes

### Kubernetes workload rightsizing

Consize reads Kubernetes workload metadata and Prometheus usage metrics, then recommends lower CPU and memory requests/limits where the current allocation is higher than observed usage.

For each workload, Consize shows:

- current CPU and memory requests/limits;
- p50, p95, p99, and max usage over time;
- the proposed request and limit;
- estimated monthly savings;
- confidence and risk;
- apply history and verification results.

> Image placeholder: workload detail page with the 14-day usage graph and the current-versus-recommended panel.

### Database rightsizing

Consize also treats database instances as managed workloads. Database recommendations use provider-scoped class catalogs, utilization caps, and maintenance-window guardrails.

For databases, Consize can recommend moving from an oversized instance class to a smaller class when CPU, memory, IOPS, and connection headroom remain within policy. Database changes move one adjacent class at a time, not multiple classes in one jump.

### Cloud-waste opportunities

Consize scans for non-workload resources that still accrue cost, such as:

- unattached storage volumes;
- stopped instances whose attached disks still cost money;
- idle load balancers;
- unused NAT gateways.

The current live cloud-waste scanner supports GCP Persistent Disk findings. Direct cleanup is intentionally narrow: Consize only requests deletion for a GCP unattached disk after re-reading the live resource and confirming it is still detached. Other cloud-waste findings should go through an IaC PR until provider-specific safety checks are implemented.

### Infrastructure-as-Code pull requests

For teams that manage infrastructure with Terraform, Kubernetes YAML, Helm, Kustomize, or GitOps, Consize should not create drift by patching the live environment behind the source of truth.

Consize therefore supports an IaC PR workflow:

1. Select a recommendation.
2. Choose “Open IaC PR.”
3. Consize updates the mapped source file in the configured GitHub repository.
4. Consize opens a draft pull request with the proposed change.
5. The team reviews, approves, merges, and lets its normal delivery system reconcile the cluster.

Terraform and Kubernetes YAML PRs are supported today. Helm values and richer GitOps mapping are planned next because charts and overlays need explicit mapping to be edited safely.

> Image placeholder: GitHub pull request created by Consize with a small resource request diff.

## The safety engine

Consize is designed around one principle: optimization should fail safe.

Before any direct apply, Consize checks guardrails. If a guardrail fails, the apply is blocked before anything is changed.

| Guardrail | What it protects |
| --- | --- |
| Store health | Consize will not apply when the audit store is unavailable. |
| Pending-only | Already applied, rejected, superseded, or resolved changes cannot be applied again by mistake. |
| Exclusions | Workloads labeled `consize.savings.dev/exclude=true` are skipped. |
| Protected namespaces | System namespaces such as `kube-system` and `consize-system` are protected. |
| Stateful/data-risk workloads | Workloads marked with data-loss risk are skipped. |
| Approval mode | Direct apply requires an authenticated operator unless the namespace explicitly allows auto-apply. |
| Step limit | Kubernetes resources move by at most 30% per apply. |
| DB class step | Databases move one adjacent class per apply. |
| Concurrency | Consize prevents overlapping applies against the same namespace. |
| Maintenance window | Database applies are blocked outside the configured window. |

Blocked applies return structured reasons so operators can see exactly why a change did not run.

```json
{
  "error": "apply blocked",
  "reasons": [
    "namespace is not auto-apply enabled",
    "recommendation has an in-flight verification"
  ]
}
```

## Apply stepping

Consize does not jump from a large allocation to a much smaller one in a single apply.

For Kubernetes CPU and memory recommendations, each direct apply moves at most 30% from the current value. The next recommendation is created only after the previous step verifies.

Example:

```text
1000m → 100m

step 1: 1000m → 700m
step 2: 700m  → 490m
step 3: 490m  → 343m
step 4: 343m  → 241m
step 5: 241m  → 169m
step 6: 169m  → 119m
step 7: 119m  → 100m
```

This creates a deliberate rhythm:

```text
apply one step → verify health → apply next step
```

When a step passes verification, Consize surfaces the queued continuation in
the apply history as the next safe action. The product can still reach the final
target, but it earns trust along the way.

## Verification

Verification is the safety check after a direct apply.

Consize compares health signals before and after the change. It is not used to decide what to recommend; it is used to prove whether a change that already happened was safe.

For Kubernetes workloads, Consize checks signals on the changed workload, not the whole namespace:

- restarts;
- OOM kills;
- evictions;
- CPU throttling;
- optional application-level latency or error-rate metrics.

For databases, Consize checks provider metrics such as CPU saturation, memory utilization, connections, IOPS, and available error signals.

Verification returns one of three verdicts:

| Verdict | Meaning |
| --- | --- |
| `passed` | The change looks safe. Consize can count the saving as realized. |
| `failed` | The change likely caused a regression. Consize rolls back and records evidence. |
| `inconclusive` | Consize does not have enough data to prove safety. It does not roll back automatically, but it requires human attention. |

The production default verification window starts at 1 hour and scales with
the apply step. Step 1 verifies after 1 hour, step 2 after 2 hours, step 3
after 3 hours, and so on. This keeps the first feedback loop fast while giving
deeper reductions more observation time.

The verifier runs every minute by default. It never verifies early; it simply
picks up applies automatically as soon as their safety window opens.

```yaml
verification:
  baseWindow: 1h
  stepScaled: true
  sustainedMinutes: 5
  signals:
    restarts: true
    oomKills: true
    evictions: true
    throttling: true
```

## Usage graphs

The workload usage graph is where engineers build confidence in a recommendation.

Consize shows observed percentiles over the analysis window, usually 14 days. The graph makes it easy to compare what the workload requested against what it actually used.

Typical interpretation:

- p50 shows normal usage;
- p95 shows the high-but-normal operating range;
- p99 and max show bursts;
- the recommendation uses headroom above observed usage rather than cutting to the exact line.

> Image placeholder: usage graph with CPU selected, showing p50/p95/p99/max and the recommendation panel beside it.

## Reports

Consize can generate savings reports for a selected time range.

Reports are useful for engineering managers, platform teams, and FinOps stakeholders who want a periodic summary without logging into the dashboard.

Current report views include:

- projected monthly savings;
- realized savings from verified applies;
- pending recommendations;
- verification outcomes;
- rollbacks;
- top open recommendations;
- recent cleanup activity.

Admins can generate a report on demand for ranges such as 7 days, 14 days, or 30 days. Weekly Slack delivery can be enabled so teams receive the report in the same channel where operational notifications are sent.

> Image placeholder: polished PDF report cover and executive summary.

## Alerting and notifications

Consize uses a contact-point and policy model for notifications.

Contact points define where alerts go, such as a Slack webhook. Policies decide which alerts are routed to which contact point. This keeps routing flexible without hard-coding one Slack channel into the deployment.

The MVP supports Slack webhook delivery for Consize events such as verification failures, rollbacks, and report delivery. Deeper incident-management features — on-call tagging, escalation schedules, Jira, ServiceNow, PagerDuty, and ownership assignment — are future integrations.

Example Slack contact point:

```yaml
alerting:
  contactPoints:
    - name: platform-slack
      type: slack
      webhookEnv: CONSIZE_SLACK_WEBHOOK
      channelLabel: "#platform-alerts"

  policies:
    - name: verification-failures
      match:
        alertname: ConsizeVerificationFailed
      contactPoint: platform-slack
```

The webhook secret should stay in Kubernetes or your secret manager. Consize stores the environment-variable reference and routing metadata, not the webhook value.

## GitHub integration

The GitHub integration lets Consize open reviewable source changes instead of forcing operators to patch live infrastructure manually.

At installation time, admins configure:

- the GitHub account or organization;
- one or more allowed repositories;
- the default branch for each repository;
- optional monorepo root paths;
- the environment variable that exposes the GitHub token.

Example:

```yaml
integrations:
  github:
    enabled: true
    tokenEnv: CONSIZE_GITHUB_TOKEN
    repositories:
      - alias: cluster
        repo: TheInvincibleRalph/Enterprise-grade-GKE-Project
        baseBranch: main
        rootPath: kubernetes/boutique
```

The token should have only the permissions required to read repository contents, create branches, write commits, and open pull requests in the configured repositories.

Consize does not need GitHub access to analyze workloads. GitHub is only required when operators want the IaC PR workflow.

## Runtime components

Consize is self-hosted and runs close to the infrastructure it manages.

| Component | Purpose |
| --- | --- |
| UI | The operator console for dashboards, recommendations, reports, alerting, and integrations. |
| API | The control plane for reads, applies, settings, auth, reports, and GitHub PR creation. |
| Postgres | The durable store for workloads, usage buckets, recommendations, applies, verification runs, cloud-waste findings, and audit history. |
| Collector | Reads Kubernetes metadata and Prometheus usage into 15-minute buckets. |
| Analyzer | Turns telemetry into recommendations. |
| Verifier | Checks applied changes and rolls back failed ones. |
| Cost scanner | Finds cloud-waste opportunities through cloud provider APIs. |
| Report job | Generates scheduled and on-demand savings reports. |
| Prometheus | Supplies workload usage and health signals. |
| Cloud APIs | Supply database metrics, cloud-waste inventory, and optional cleanup operations. |
| GitHub | Receives IaC pull requests when that workflow is enabled. |

## Runtime topology

Typical production topology:

```text
Kubernetes cluster
├─ consize-ui
├─ consize-api
├─ consize-collector CronJob
├─ consize-analyze CronJob
├─ consize-verify CronJob
├─ consize-costscan CronJob
└─ consize-report CronJob

External or managed dependencies
├─ Postgres
├─ Prometheus or compatible query endpoint
├─ cloud provider APIs
└─ GitHub / Slack integrations
```

Consize can be installed per team, per namespace group, or cluster-wide.

For a team-scoped installation, set `CONSIZE_NAMESPACES` to the namespaces that team owns. For a cluster-wide read installation, leave it empty and bind the read-only service account cluster-wide. Direct write access remains separately scoped by namespace RoleBindings.

## Installation

### Prerequisites

You need:

- a Kubernetes cluster;
- Prometheus or a compatible query endpoint with workload metrics;

- optional cloud provider credentials for database metrics and cloud-waste scanning;
- optional GitHub and Slack secrets.

### 1. Create the namespace

```sh
kubectl create namespace consize-system
```

### 2. Create required secrets

```sh
kubectl -n consize-system create secret generic consize-store \
  --from-literal=prometheus-url='http://prometheus-operated.monitoring:9090'
```

> Consize provisions a lightweight Postgres database automatically. To use an external Postgres database, set `postgresql.enabled=false` in your values and provide a `database-url` in the secret.

Optional Slack webhook:

```sh
kubectl -n consize-system create secret generic consize-alerts \
  --from-literal=slack-webhook='https://hooks.slack.com/services/...'
```

Optional GitHub token:

```sh
kubectl -n consize-system create secret generic consize-github \
  --from-literal=token='github_pat_...'
```

Optional Cloud IAM credentials for Cloud SQL metrics and cloud-waste scanning:

> Even though Consize runs inside your cluster, Kubernetes pods only have cluster-level permissions by default. You still need cloud-level IAM permissions to scan out-of-cluster infrastructure like unattached EBS volumes or managed databases.

The most secure and preferred method is to use **Workload Identity (GCP)** or **IRSA (AWS)**. The Consize Helm chart automatically creates the Kubernetes ServiceAccounts for you. You simply annotate them in your `values.yaml` to bind them to your Cloud IAM roles:

```yaml
serviceAccounts:
  reader:
    annotations:
      iam.gke.io/gcp-service-account: consize-scanner@PROJECT_ID.iam.gserviceaccount.com
      # or for AWS:
      # eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/consize-scanner
```

<details>
<summary><strong>How to create the GCP Workload Identity Role (CLI)</strong></summary>

If you haven't created the Cloud IAM role yet, you can do so quickly using the `gcloud` CLI. This creates the role, grants the exact read-only permissions Consize needs, and binds it to the Kubernetes ServiceAccount that Helm will create (`consize-system/consize-reader`).

```sh
# 1. Create the Google Cloud Service Account (GSA)
gcloud iam service-accounts create consize-scanner --project=PROJECT_ID

# 2. Grant the GSA the specific read-only permissions for Cloud Waste and Cloud SQL
gcloud projects add-iam-policy-binding PROJECT_ID \
    --member="serviceAccount:consize-scanner@PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/compute.viewer"
gcloud projects add-iam-policy-binding PROJECT_ID \
    --member="serviceAccount:consize-scanner@PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/cloudsql.viewer"

# 3. Bind the GSA to the Kubernetes ServiceAccount (KSA)
gcloud iam service-accounts add-iam-policy-binding consize-scanner@PROJECT_ID.iam.gserviceaccount.com \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:PROJECT_ID.svc.id.goog[consize-system/consize-reader]" \
    --project=PROJECT_ID
```
</details>

<details>
<summary><strong>How to create the AWS IRSA Role (CLI)</strong></summary>

If you are running on EKS, you can use the `aws` and `eksctl` CLIs to create an IAM role with the exact read-only permissions required, without conflicting with the Helm chart.

```sh
# 1. Create the IAM policy with required read-only permissions
cat <<POLICY > consize-policy.json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ec2:DescribeInstances",
                "ec2:DescribeVolumes",
                "ec2:DescribeAddresses",
                "rds:DescribeDBInstances"
            ],
            "Resource": "*"
        }
    ]
}
POLICY

aws iam create-policy \
    --policy-name ConsizeCloudScannerPolicy \
    --policy-document file://consize-policy.json

# 2. Create the IAM Role tied to the Consize ServiceAccount (using --role-only to let Helm manage the K8s SA)
eksctl create iamserviceaccount \
    --name consize-reader \
    --namespace consize-system \
    --cluster YOUR_CLUSTER_NAME \
    --attach-policy-arn arn:aws:iam::YOUR_ACCOUNT_ID:policy/ConsizeCloudScannerPolicy \
    --approve \
    --role-only
```
</details>


Alternatively, if you do not use Workload Identity, you can provide a static JSON key file via a secret:

```sh
kubectl -n consize-system create secret generic consize-gcp \
  --from-file=key.json=./consize-gcp-service-account.json
```

### 3. Configure collection scope

`CONSIZE_NAMESPACES` tells the collector which Kubernetes namespaces this installation should read and analyze.

```yaml
env:
  CONSIZE_NAMESPACES: boutique,payments,checkout
```

That configuration means Consize only collects workloads and usage for those namespaces. It ignores the rest of the cluster.

Leave it empty for cluster-wide discovery:

```yaml
env:
  CONSIZE_NAMESPACES: ""
```

Cluster-wide discovery is read-only. It does not mean cluster-wide write access. Direct apply still requires namespace-specific write RBAC.

### 4. Apply read RBAC

For cluster-wide discovery:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: consize-reader
  namespace: consize-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: consize-read
rules:
  - apiGroups: [""]
    resources: ["namespaces", "pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "daemonsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: consize-read
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: consize-read
subjects:
  - kind: ServiceAccount
    name: consize-reader
    namespace: consize-system
```

For namespace-scoped installations, bind equivalent read permissions only in the selected namespaces.

### 5. Enable direct apply only where allowed

Direct apply uses a separate write identity. Bind it only in namespaces where operators are allowed to patch workloads.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: consize-writer
  namespace: consize-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: consize-apply
  namespace: boutique
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: consize-apply
  namespace: boutique
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: consize-apply
subjects:
  - kind: ServiceAccount
    name: consize-writer
    namespace: consize-system
```

To allow automatic apply in that namespace, add the namespace label:

```sh
kubectl label namespace boutique consize.savings.dev/auto-apply=enabled
```

Without that label, Consize can still do approved direct applies, but not automatic applies.

### 6. Deploy Consize

Helm is the recommended production installation path. The chart renders the API, service accounts, least-privilege RBAC, migration job, and CronJobs for collection, analysis, verification, cloud-waste scanning, and reporting.

```sh
helm upgrade --install consize ./charts/consize \
  --namespace consize-system \
  --create-namespace \
  -f ./charts/consize/examples/values-prod.yaml
```

Production values should reference existing Kubernetes Secrets. Do not put raw tokens, database passwords, Slack webhooks, or service-account JSON in `values.yaml`.

Cluster-wide read-only discovery with direct apply allowed only in `boutique`:

```yaml
collector:
  namespaces: []

rbac:
  writer:
    namespaces:
      - boutique
```

Team-scoped installation:

```yaml
collector:
  namespaces:
    - boutique
    - checkout

rbac:
  writer:
    namespaces:
      - boutique
```

### 7. Verify the installation

```sh
kubectl -n consize-system get pods
kubectl -n consize-system get cronjobs
kubectl -n consize-system port-forward svc/consize-api 18099:8080
curl http://127.0.0.1:18099/readyz
```

Expected response:

```json
{"status":"ready"}
```

## Configuration reference

| Setting | Example | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://...` | Postgres store for telemetry, recommendations, settings, and audit history. |
| `PROMETHEUS_URL` | `http://prometheus:9090` | Prometheus query endpoint for usage and health signals. |
| `CONSIZE_NAMESPACES` | `boutique,payments` | Namespace scope for collection. Empty means cluster-wide read discovery. |
| `CONSIZE_VERIFY_WINDOW` | `1h` | Base post-apply verification window. Effective window is `base × step number`. |
| `CONSIZE_SUSTAINED_MINUTES` | `5` | How long a regression signal must persist before it is treated as failed. |
| `CONSIZE_MIN_DATA_DAYS` | `5` | Minimum telemetry history before generating recommendations. |
| `CONSIZE_PRICING` | `static` | Pricing source. |
| `CONSIZE_DBMETRICS` | `gcp` | Database metrics source. |
| `CONSIZE_COSTSCAN` | `gcp` | Cloud-waste scanner source. Use `none` to disable. |
| `CONSIZE_GCP_PROJECT` | `my-project` | GCP project for Cloud SQL metrics and cloud-waste scanning. |
| `GOOGLE_APPLICATION_CREDENTIALS` | `/etc/consize-gcp/key.json` | Mounted service-account key path. |
| `CONSIZE_AUTH_REQUIRED` | `true` | Enables server-side user authentication and role enforcement. |
| `CONSIZE_GITHUB_TOKEN` | secret env var | Token used to open GitHub IaC pull requests. |
| `CONSIZE_SLACK_WEBHOOK` | secret env var | Slack webhook used by contact points and reports. |

## Security model

Consize separates read, write, and integration permissions.

- The reader service account discovers workloads and usage metadata.
- The writer service account is only bound in namespaces where direct apply is allowed.
- GitHub permissions are used only for source-control PRs.
- Slack webhooks and provider credentials stay in Kubernetes Secrets or a secret manager.
- Authentication and role checks are enforced on the server.
- Every direct apply writes an audit record.
- Cloud-waste direct cleanup writes its own action history.
- Verification failures trigger rollback and evidence capture.

This gives teams a practical operating model: review by default, direct apply where explicitly allowed, and no silent mutation.

## Operating model for teams

Most teams start in review-only mode:

1. Install Consize with read-only Kubernetes permissions.
2. Let it collect enough telemetry.
3. Review recommendations in the dashboard.
4. Connect GitHub.
5. Open IaC PRs for safe, reviewable source changes.

After trust is established, teams can enable direct apply in selected namespaces:

1. Add the `consize-apply` RoleBinding only to approved namespaces.
2. Keep auto-apply disabled at first.
3. Use approved direct applies for low-risk recommendations.
4. Review verification results and rollback behavior.
5. Enable `consize.savings.dev/auto-apply=enabled` only where the team is comfortable with policy-driven changes.

## What Consize is not

Consize is not a general autoscaler replacement. It does not tune HPA behavior, schedule pods, or replace your deployment system.

It is also not a billing warehouse. It uses pricing and telemetry to estimate savings, but its core job is operational: find waste, propose a safe change, help the team apply it, verify the outcome, and keep evidence.

## Current scope and roadmap

Current product scope:

- Kubernetes CPU and memory rightsizing;
- database class recommendations;
- direct apply with guardrails;
- verification and rollback;
- audit trail;
- Slack notification contact points;
- weekly and on-demand reports;
- GitHub IaC PR creation;
- GCP cloud-waste scanning for unattached disks;
- direct cleanup for confirmed unattached GCP Persistent Disks.

Planned next:

- broader cloud-waste detection for idle load balancers and unused NAT gateways using traffic evidence;
- GitOps manifest PRs for Helm values, Kustomize overlays, and ArgoCD-managed applications;
- richer repository discovery and workload-to-source mapping;
- on-call tagging and incident-management integrations;
- Jira, ServiceNow, PagerDuty, and enterprise approval workflows;
- multi-cloud cloud-waste scanners;
- signed image releases and OCI chart publishing.

## The short version

Consize gives platform teams a safer way to reduce infrastructure cost.

It finds oversized workloads, oversized databases, and cloud resources that should not still be billing. It turns findings into reviewable changes or guarded direct applies. Then it verifies production health and records the evidence.

That is the difference between cost visibility and cost action.
