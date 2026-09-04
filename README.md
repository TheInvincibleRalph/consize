# Consize

> **The automated safety engine for cloud cost optimization.**

Cloud infrastructure teams waste 30–50% of their compute and database spend not because they lack visibility, but because the operational risk outweighs the potential savings.

**Consize exists to close that gap.** It is a fully automated safety engine that analyzes your infrastructure, applies rightsizing changes in small steps, verifies the safety of those changes in real-time, and automatically rolls back if things go wrong. 

## 🚀 Try the Interactive Sandbox

The fastest way to experience Consize's safety net is through the Interactive Sandbox. It runs entirely on your local machine using a single Docker container, pre-seeded with historical data, cloud waste opportunities, and a live metrics simulation.

```bash
# Pull and run the all-in-one interactive sandbox
docker run -p 3000:3000 -p 8080:8080 -it ghcr.io/theinvincibleralph/consize-sandbox:latest
```

Open `http://localhost:3000` in your browser. 
You can watch the Verifier catch an intentional regression on the `checkout-api` workload and instantly trigger an automatic rollback to restore safety.

## 📦 Production Installation

Consize is built to run safely inside your cluster. It uses read-only access for analysis and requires explicit, least-privilege, namespace-scoped RoleBindings before it can apply any changes.

Install Consize onto a live cluster (AWS and GCP currently supported) using our official Helm chart:

```bash
git clone https://github.com/TheInvincibleRalph/consize.git
cd consize

helm install consize ./charts/consize \
  --namespace consize-system \
  --create-namespace \
  -f charts/consize/examples/values-prod.yaml
```

## 🛠 Features

- **Kubernetes Rightsizing (CPU & Memory):** Deterministic p95/p99 usage analysis over 14-day windows.
- **Cloud Waste Scanning (AWS & GCP):** Automatically detects unattached EBS volumes, unattached Elastic IPs, stopped EC2/Compute Engine instances, and more.
- **Direct Cleanup & IaC PRs:** Clean up waste directly through the UI or automatically generate Pull Requests against your Infrastructure-as-Code repository (Terraform, YAML).
- **Step-wise Apply:** Large changes are never applied at once; they are broken down into smaller, safe increments.
- **Auto-rollback Guardrails:** Monitors SLIs (OOMKills, CPU throttling) after every change. If a threshold is breached, Consize instantly rolls back to the byte-identical pre-apply state.

## 📚 Documentation

For a deeper dive into how Consize works, check out the documentation:

- [Vision & The Safety Net](VISION.md)
- [Customer Guide & Operating Model](docs/customer-guide.md)
- [Architecture & Data Flow](docs/architecture.md)
- [Security & Least Privilege](SECURITY.md)
- [Decisions & ADR Log](docs/decisions.md)

## License

MIT
