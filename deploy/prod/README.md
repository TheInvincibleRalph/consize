# Consize prod

Production is the live GKE installation. These helpers wrap the existing
manifests in `engine/deploy/` with context checks so local development does not
silently talk to or mutate prod.

- GCP project: `devops-portfolio-prod`
- GKE cluster: `devops-portfolio`
- Region: `us-central1`
- Namespace: `consize-system`
- API service: `consize-api`
- Local prod port-forward: `127.0.0.1:18099`

## One-time setup

```sh
cp deploy/prod/env.sh.example deploy/prod/env.sh
gcloud auth login admin@invincibledevops.tech
gcloud config set project devops-portfolio-prod
gcloud container clusters get-credentials devops-portfolio \
  --region us-central1 \
  --project devops-portfolio-prod
```

## Check prod context

```sh
./deploy/prod/check-context.sh
```

If this fails, do not deploy. Fix `gcloud` auth, project, or kube context first.

## Bring the API into the local browser

```sh
./deploy/prod/port-forward-api.sh
```

Then run the UI explicitly against prod:

```sh
cd ui
API_UPSTREAM=http://127.0.0.1:18099 npm run dev
```

## Deploy an API image

Use an immutable tag, never `latest`:

```sh
./deploy/prod/deploy-api.sh consize-realized-verified-20260828
```

The script checks the GCP project, kube context, namespace, and service before it
touches the deployment.
