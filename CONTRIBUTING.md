# Contributing to Consize

First off, thank you for considering contributing to Consize! People like you make this open-source tool a reality.

## Local Development

You do not need a Kubernetes cluster or a cloud provider to develop Consize! We have built a fully self-contained sandboxed demo using Docker Compose that spins up the UI, API, Verifier, and a stubbed Prometheus.

### Prerequisites
- Docker & Docker Compose
- Go 1.22+
- Node.js 20+

### Running the Sandbox

To spin up the entire stack locally:
```bash
docker compose -f docker-compose.demo.yaml up --build
```
The dashboard will be available at `http://localhost:3000`. Hot-reloading is enabled for the UI.

## Pull Request Process
1. Fork the repo and create your branch from `main`.
2. If you've added code that should be tested, add unit tests.
3. Ensure the test suite passes: `cd engine && go test ./...`
4. Make sure your code passes the linter: `golangci-lint run`
5. Issue a Pull Request using our standard PR template.
