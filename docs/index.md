# Consize

Consize is an open-source engine for **Safe, Automated Action** in Kubernetes. 

While tools like Kubecost provide excellent observability and cost allocation, they leave the actual remediation to the operator. Consize closes this loop by automating the application of resource changes, heavily gated by an automated rollback engine driven by real-time SLIs.

## Core Philosophy

We believe automation must be safe. Consize never applies a change without a mathematical guarantee that it can verify the result and roll it back if necessary.

1. **Deterministic Action:** No black-box AI applied directly to your cluster. Every change is an explicitly approved patch to a Kubernetes resource.
2. **Verification Loop:** Consize watches your Prometheus metrics (like `OOMKills`, `CPUThrottling`, or `Latency`) immediately after a change.
3. **Automated Rollback:** If an SLI breaches its threshold during the verification window, the change is instantly reverted.

## Quick Start

You can try Consize entirely locally using our interactive sandboxed demo. It includes a mock Prometheus instance, a test database, and the full verify/rollback engine.

```bash
git clone https://github.com/consize-oss/consize.git
cd consize
docker compose -f docker-compose.demo.yaml up --build
```

Then open `http://localhost:3000` to access the dashboard.

## Getting Help

- Join our Discord server.
- Open an issue on GitHub.
