#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/env.sh"

if [ ! -f "$ENV_FILE" ]; then
  ENV_FILE="$SCRIPT_DIR/env.sh.example"
fi

set -a
. "$ENV_FILE"
set +a

fail() {
  printf 'prod check failed: %s\n' "$*" >&2
  exit 1
}

command -v gcloud >/dev/null || fail "gcloud is not installed"
command -v kubectl >/dev/null || fail "kubectl is not installed"

ACTIVE_PROJECT="$(gcloud config get-value project 2>/dev/null || true)"
if [ "$ACTIVE_PROJECT" != "$CONSIZE_GCP_PROJECT" ]; then
  fail "gcloud project is '$ACTIVE_PROJECT', expected '$CONSIZE_GCP_PROJECT'"
fi

CURRENT_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
if [ "$CURRENT_CONTEXT" != "$CONSIZE_KUBE_CONTEXT" ]; then
  fail "kubectl context is '$CURRENT_CONTEXT', expected '$CONSIZE_KUBE_CONTEXT'"
fi

kubectl get namespace "$CONSIZE_NAMESPACE" >/dev/null
kubectl -n "$CONSIZE_NAMESPACE" get svc "$CONSIZE_API_SERVICE" >/dev/null

printf 'prod context ok: %s / %s / namespace %s\n' \
  "$CONSIZE_GCP_PROJECT" "$CONSIZE_KUBE_CONTEXT" "$CONSIZE_NAMESPACE"
