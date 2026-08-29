# Consize local dev

Local dev is intentionally separate from the live GKE installation.

- API: `http://127.0.0.1:8080`
- UI: `http://127.0.0.1:3000`
- Postgres: `127.0.0.1:15432`
- Default sources: fixture DB metrics and fixture cloud-waste scan

## First run

```sh
cp deploy/dev/.env.example deploy/dev/.env
./deploy/dev/start-db.sh
./deploy/dev/start-api.sh
```

In another terminal:

```sh
./deploy/dev/start-ui.sh
```

## Seed or refresh local data

With the dev database and API environment configured:

```sh
./deploy/dev/run-cycle.sh
```

This runs collector, analyzer, and cloud-waste scan once against the configured
dev sources. The default source choices are safe local fixtures and do not read
the live cluster.

## When to point dev at real infrastructure

Only set `CONSIZE_KUBECONFIG`, `PROMETHEUS_URL`, `CONSIZE_DBMETRICS=gcp`, or
`CONSIZE_COSTSCAN=gcp` in `deploy/dev/.env` when you deliberately want local dev
to read a throwaway or test cluster. Production GKE access belongs under
`deploy/prod/`.
