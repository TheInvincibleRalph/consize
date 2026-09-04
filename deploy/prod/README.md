# Consize prod

Production is the live GKE installation. **We have migrated to using Helm** to deploy and manage Consize, utilizing GKE Workload Identity for a secure, keyless integration with GCP.

- GCP project: `NEW_PROJECT_ID_HERE`
- GKE cluster: `consize-prod-cluster` (Example)
- Region: `us-central1`
- Namespace: `consize-system`
- Local prod port-forward: `127.0.0.1:18099`

## One-time setup (New GCP Cluster)

First, authenticate and get your cluster credentials:

```sh
gcloud auth login
gcloud config set project NEW_PROJECT_ID_HERE
gcloud container clusters get-credentials consize-prod-cluster \
  --region us-central1 \
  --project NEW_PROJECT_ID_HERE
```

Next, create the `consize-system` namespace and the required integration secrets:

```sh
kubectl create namespace consize-system

# Store (Required)
kubectl -n consize-system create secret generic consize-store \
  --from-literal=database-url='postgres://USER:PASSWORD@HOST:5432/consize?sslmode=require' \
  --from-literal=prometheus-url='http://prometheus-operated.monitoring:9090'

# GitHub (Optional, for IaC PR workflow)
kubectl -n consize-system create secret generic consize-github \
  --from-literal=token="$CONSIZE_GITHUB_TOKEN"

# Slack (Optional, for alerts)
kubectl -n consize-system create secret generic consize-alerts \
  --from-literal=slack-webhook='https://hooks.slack.com/services/...'
```

*(Note: We no longer create `consize-gcp` JSON keys. The Helm chart now annotates our ServiceAccounts for GKE Workload Identity!)*

## Install / Upgrade via Helm

Update your `values-new-gcp-cluster.yaml` (located in `charts/consize/examples/`) with your new Project ID, then deploy:

```sh
helm upgrade --install consize ./charts/consize \
  --namespace consize-system \
  --create-namespace \
  -f ./charts/consize/examples/values-new-gcp-cluster.yaml
```

## Bring the API into the local browser

```sh
kubectl -n consize-system port-forward svc/consize-api 18099:8080
```

Then run the UI explicitly against prod:

```sh
cd ui
API_UPSTREAM=http://127.0.0.1:18099 npm run dev
```

## GitHub IaC PR workflow

Configure the GitHub integration in the UI:

- organization/account: `TheInvincibleRalph`
- token env var: `CONSIZE_GITHUB_TOKEN`
- repository alias: `cluster`
- repository: `Enterprise-grade-GKE-Project`
- base branch: the production GitOps branch, usually `main` or `master`
- optional root path: only for monorepos

## Scoped direct writes

Production Direct apply is opt-in per namespace. To allow Direct apply in a
namespace, create a namespaced Role and RoleBinding for `consize-writer`:

```sh
kubectl create role consize-apply -n <namespace> \
  --verb=get,list,watch,update --resource=deployments

kubectl create rolebinding consize-apply -n <namespace> \
  --serviceaccount=consize-system:consize-writer \
  --role=consize-apply
```

For automatic apply in that namespace, add the extra guardrail label:

```sh
kubectl label namespace <namespace> consize.savings.dev/auto-apply=enabled
```

IaC PR mode does not need Kubernetes write RBAC; it uses the configured GitHub
integration to open reviewable source changes.
