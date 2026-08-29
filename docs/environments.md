# Dev and prod environments

Consize now has two explicit operating modes:

| Environment | Purpose | Data source | Default API | Risk posture |
| --- | --- | --- | --- | --- |
| Dev | Local product work and safe experiments | Local Postgres + fixtures by default | `127.0.0.1:8080` | No live cloud mutation |
| Prod | Live GKE installation | Real cluster telemetry and cloud adapters | `127.0.0.1:18099` through port-forward | Guarded, explicit, context-checked |

## Dev

Use dev for UI work, API changes, report rendering, and product iteration:

```sh
cp deploy/dev/.env.example deploy/dev/.env
./deploy/dev/start-db.sh
./deploy/dev/start-api.sh
./deploy/dev/start-ui.sh
```

The dev UI proxies `/api/v1/*` to the local API on `8080`.

## Prod

Use prod only when validating the live GKE installation:

```sh
cp deploy/prod/env.sh.example deploy/prod/env.sh
./deploy/prod/check-context.sh
./deploy/prod/port-forward-api.sh
```

To view prod from the local UI, opt in explicitly:

```sh
cd ui
API_UPSTREAM=http://127.0.0.1:18099 npm run dev
```

## Rule of thumb

If the task is design, copy, API contract work, report formatting, or tests, use
dev. If the task is proving real telemetry, running collector/analyzer/verifier
against GKE, or validating a deployed image, use prod.
