<div align="center">

  <img src="docs/assets/banner.jpg" alt="Consize Logo" width="800"/>

# Consize

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg?logo=docker)](https://github.com/TheInvincibleRalph/consize-sandbox)

</div>

**The automated safety engine for cloud cost optimization.**

Cloud infrastructure teams waste 30–50% of their compute and database spend not because they lack visibility, but because the operational risk outweighs the potential savings.

**Consize exists to close that gap.** It is a fully automated safety engine that analyzes your infrastructure, applies rightsizing changes in small steps, verifies the safety of those changes in real-time, and automatically rolls back if things go wrong.

---

## Try the Interactive Sandbox

The fastest way to experience Consize's safety net is through our **Interactive Sandbox**. It runs entirely on your local machine using a single Docker container, pre-seeded with historical data, cloud waste opportunities, and a live metrics simulation.

```bash
# Pull and run the all-in-one interactive sandbox
docker run -p 3000:3000 -p 8080:8080 -it ghcr.io/theinvincibleralph/consize-sandbox:latest
```

Open `http://localhost:3000` in your browser. You can watch the Verifier catch an intentional regression on the `checkout-api` workload and instantly trigger an automatic rollback to restore safety.

<img src="img/demo-dashboard.png" alt="Consize Demo Dashboard" />

---

## Production Installation

Consize is built to run safely inside your cluster. It uses read-only access for analysis and requires explicit, least-privilege, namespace-scoped `RoleBindings` before it can apply any changes.

Install Consize onto a live cluster (AWS and GCP currently supported) using our official Helm chart hosted on GitHub Container Registry (GHCR):

```bash
# 1. Export the default values to customize your installation
helm show values oci://ghcr.io/theinvincibleralph/charts/consize > values.yaml

# 2. Install the chart using your customized values
helm install consize oci://ghcr.io/theinvincibleralph/charts/consize \
  --version 0.1.0 \
  --namespace consize-system \
  --create-namespace \
  -f values.yaml
```

---

## Configuration

Consize uses the standard Kubernetes pattern: credentials live in **Secrets**, and scoping lives in **`values.yaml`**. We deliberately separate read scope from write scope for maximum safety.

### 1. Required Secrets
Before installing, you must provide your Postgres and Prometheus connection strings:
```bash
kubectl create namespace consize-system

kubectl -n consize-system create secret generic consize-store \
  --from-literal=database-url='postgres://USER:PASSWORD@HOST:5432/consize?sslmode=require' \
  --from-literal=prometheus-url='http://prometheus-operated.monitoring:9090'
```

### 2. Service Accounts & RBAC (`values.yaml`)
Consize uses two distinct ServiceAccounts: a `reader` for scanning workloads and a `writer` for applying patches and rollbacks. 

You explicitly control where Consize can act. For example, to read the whole cluster but only allow changes in the `boutique` namespace:
```yaml
# Read cluster-wide
collector:
  namespaces: []

# Only allow writes (apply/rollback) in the boutique namespace
rbac:
  writer:
    namespaces:
      - boutique
```

### 3. Integrations (Slack, GitHub, Cloud Waste)
Optional features require their respective secrets:

```bash
# For IaC PR workflows
kubectl -n consize-system create secret generic consize-github \
  --from-literal=token='github_pat_...'

# For Cloud Waste scanning (GCP)
kubectl -n consize-system create secret generic consize-gcp \
  --from-file=key.json=./consize-gcp-service-account.json
```
Then reference them in `values.yaml`:
```yaml
github:
  existingSecret: consize-github
cloudWaste:
  enabled: true
  provider: gcp
```

For the complete list of Helm values and deployment modes, see the **[Helm Chart Documentation](charts/consize/README.md)**.

---

## Core Features

| Feature | Description |
|---------|-------------|
| **Kubernetes Rightsizing** | Deterministic CPU & Memory p95/p99 usage analysis over 14-day windows. |
| **Cloud Waste Scanning** | Automatically detects unattached EBS volumes, Elastic IPs, and stopped Compute instances (AWS/GCP). |
| **IaC Integration** | Clean up waste directly through the UI or automatically generate PRs against your GitOps repos (Terraform/YAML). |
| **Step-wise Apply** | Large changes are never applied at once; they are broken down into smaller, safe increments. |
| **Auto-Rollback Guardrails** | Monitors SLIs (OOMKills, CPU throttling) after every change. Breaching a threshold triggers an instant, byte-identical rollback. |

---

## Documentation

For a deeper dive into how Consize works under the hood, explore our documentation:

- **[Vision & The Safety Net](VISION.md)**
- **[Customer Guide & Operating Model](docs/customer-guide.md)**
- **[Architecture & Data Flow](docs/architecture.md)**
- **[Security & Least Privilege](SECURITY.md)**
- **[Decisions & ADR Log](docs/decisions.md)**

---

## Contributing

Consize is completely open for contribution! We build with the community, not just for the community. 

Whether it's adding support for new cloud providers, fixing bugs, or improving documentation, we welcome all contributions. 

- **Good first issues:** Look for the `good first issue` label on our issue tracker to get started.
- **Design proposals:** For larger architectural changes, please open a GitHub Discussion first. We use Architecture Decision Records (ADRs) to document significant decisions.
- **Local Development:** Check out the [Architecture & Data Flow](docs/architecture.md) documentation to understand how the components fit together before spinning up your local environment.

---

## ⚖️ License

Distributed under the **Apache 2.0 License**. See [`LICENSE`](LICENSE) for more information.
