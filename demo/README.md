# Consize Interactive Sandbox

This is a fully self-contained interactive demo of Consize. It spins up the complete backend (API, Verifier, Prometheus Stub, Postgres) and the Next.js UI in a single container.

### Running the Sandbox

You don't need a Kubernetes cluster or any external dependencies to run this. Just run:

```bash
docker run -p 3000:3000 ghcr.io/theinvincibleralph/consize-sandbox:latest
```

Then open [http://localhost:3000](http://localhost:3000) in your browser.

### How the Demo Works

To show the core value of Consize (the Safety Net), this sandbox is pre-configured with a fake `prometheus-stub` and an `injector`. 

When you apply a recommendation (like `checkout-api`), here is what happens:
1. The **API** records the change and simulates patching the cluster in-memory.
2. The **Verifier** begins aggressively monitoring the metrics for the next 5 minutes.
3. 15 seconds into the verification, the **Injector** triggers the `prometheus-stub` to suddenly spike OOMKill metrics for that specific workload.
4. The **Verifier** catches the SLI breach and instantly auto-rollbacks the workload, marking the recommendation as `failed` and restoring safety.

This allows you to see the exact workflow and safety guarantees without risking real infrastructure.
