# Consize Helm chart

This chart installs Consize into the Kubernetes cluster it manages.

Consize uses the same installation shape expected from production Kubernetes tools: operators create Secrets for credentials, choose a scope in `values.yaml`, then install or upgrade with Helm.

## What gets installed

- API Deployment for the Consize backend.
- UI Deployment and Service for the dashboard.
- Collector CronJob for Kubernetes workload and usage ingestion.
- Analyzer CronJob for recommendation generation.
- Verifier CronJob for post-apply safety checks and rollback.
- Report CronJob for scheduled savings reports.
- Optional cloud-waste scanner CronJob.
- A migration Job for database schema upgrades.
- Reader and writer ServiceAccounts with separate RBAC scopes.

## Install

Create the required store secret:

```sh
kubectl create namespace consize-system

kubectl -n consize-system create secret generic consize-store \
  --from-literal=database-url='postgres://USER:PASSWORD@HOST:5432/consize?sslmode=require' \
  --from-literal=prometheus-url='http://prometheus-operated.monitoring:9090'
```

Optional integration secrets:

```sh
kubectl -n consize-system create secret generic consize-github \
  --from-literal=token='github_pat_...'

kubectl -n consize-system create secret generic consize-alerts \
  --from-literal=slack-webhook='https://hooks.slack.com/services/...'

kubectl -n consize-system create secret generic consize-gcp \
  --from-file=key.json=./consize-gcp-service-account.json
```

Install:

```sh
helm upgrade --install consize ./charts/consize \
  --namespace consize-system \
  --create-namespace \
  -f ./charts/consize/examples/values-prod.yaml
```

Verify:

```sh
kubectl -n consize-system get deploy,svc,cronjob
kubectl -n consize-system port-forward svc/consize-api 18099:8080
curl http://127.0.0.1:18099/readyz
```

The chart also prints these commands through Helm notes after installation.

## Scoping model

Consize deliberately separates read scope from write scope.

- `collector.namespaces: []` means cluster-wide read-only discovery.
- `collector.namespaces: ["boutique"]` means read only that namespace.
- `rbac.writer.namespaces` controls where Direct apply and rollback can update Deployments.
- IaC PR mode does not need Kubernetes write RBAC.

Cluster-wide read with direct apply only in `boutique`:

```yaml
collector:
  namespaces: []

rbac:
  writer:
    namespaces:
      - boutique
```

Team-scoped install:

```yaml
collector:
  namespaces:
    - boutique

rbac:
  writer:
    namespaces:
      - boutique
```

Read-only / PR-only install:

```yaml
rbac:
  writer:
    namespaces: []
```

## Install modes

### Read-only / IaC PR only

Use this when teams do not want Consize to patch Kubernetes directly. Consize still reads workloads, analyzes telemetry, and can open reviewable GitHub PRs.

```yaml
collector:
  namespaces: []

rbac:
  writer:
    namespaces: []

github:
  existingSecret: consize-github
```

### Team-scoped direct apply

Use this when one team installs Consize for only the namespaces it owns.

```yaml
collector:
  namespaces:
    - boutique

rbac:
  writer:
    namespaces:
      - boutique
```

### Enterprise read with scoped write

Use this when a platform team wants cluster-wide recommendations, but direct apply remains limited to approved namespaces.

```yaml
collector:
  namespaces: []

rbac:
  writer:
    namespaces:
      - boutique
      - payments
```

### GCP cloud-waste scanning

Cloud-waste scanning is disabled by default so production installs do not seed fixtures or create a scanner without credentials.

```yaml
cloudWaste:
  enabled: true
  provider: gcp

gcp:
  project: devops-portfolio-prod
```

For JSON key credentials:

```sh
kubectl -n consize-system create secret generic consize-gcp \
  --from-file=key.json=./consize-gcp-service-account.json
```

```yaml
gcp:
  credentials:
    existingSecret: consize-gcp
    key: key.json
```

For GKE Workload Identity, annotate the Consize ServiceAccounts instead of mounting a JSON key:

```yaml
serviceAccounts:
  reader:
    annotations:
      iam.gke.io/gcp-service-account: consize-scanner@PROJECT_ID.iam.gserviceaccount.com
  writer:
    annotations:
      iam.gke.io/gcp-service-account: consize-writer@PROJECT_ID.iam.gserviceaccount.com

gcp:
  project: PROJECT_ID
```

## Production defaults

The chart defaults to:

- authentication enabled;
- no fixture sources;
- no cluster-wide write permission;
- collector every 15 minutes;
- analyzer every 15 minutes;
- verifier every minute, with due applies gated by the configured safety window;
- weekly report CronJob enabled, with delivery controlled by app settings;
- cloud-waste scanner disabled until `cloudWaste.provider` is set.

The verifier window is intentionally configurable. The default base window is `1h`; Consize scales the wait by step number so a later step collects more evidence before verification.

## Integration secrets

Consize stores routing metadata in the database, but sensitive integration values stay in Kubernetes Secrets.

GitHub token for IaC PRs:

```yaml
github:
  existingSecret: consize-github
  key: token
```

Slack webhook for alert tests, verification alerts, and report delivery:

```yaml
slack:
  existingSecret: consize-alerts
  key: slack-webhook
```

Private image registry pull secrets:

```yaml
global:
  imagePullSecrets:
    - artifact-registry-pull
```

Additional platform annotations can be applied through:

```yaml
commonLabels:
  platform.example.com/owner: finops

api:
  podAnnotations:
    cluster-autoscaler.kubernetes.io/safe-to-evict: "true"
```

## Main values

| Value | Purpose |
| --- | --- |
| `global.imageRegistry` / `global.imageTag` | Image location and tag for all Consize binaries. |
| `global.imagePullSecrets` | Private registry pull secrets used by all Consize pods. |
| `commonLabels` | Extra labels stamped across rendered resources. |
| `database.existingSecret` | Secret containing `DATABASE_URL`. |
| `prometheus.existingSecret` | Secret containing `PROMETHEUS_URL`. |
| `collector.namespaces` | Namespace read scope. Empty means cluster-wide read discovery. |
| `rbac.writer.namespaces` | Namespaces where Direct apply and rollback are allowed. |
| `github.existingSecret` | Secret containing the GitHub token used to open IaC PRs. |
| `slack.existingSecret` | Secret containing the Slack webhook for alerts and reports. |
| `gcp.project` / `gcp.credentials.existingSecret` | GCP project and credential mount for Cloud SQL metrics and cloud-waste scanning. |
| `verify.window` | Base verification window. Effective wait scales by apply step number. |
| `analyze.minDataDays` | Minimum telemetry history before recommendations are created. |

## Validate locally

```sh
helm lint charts/consize
helm template consize charts/consize -f charts/consize/examples/values-team-scoped.yaml
helm template consize charts/consize -f charts/consize/examples/values-prod.yaml
```

Invalid combinations are rejected at render time. For example, `cloudWaste.enabled=true` with `cloudWaste.provider=none` fails before Kubernetes receives any resources.
