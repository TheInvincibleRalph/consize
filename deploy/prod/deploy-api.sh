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

TAG="${1:-}"
if [ -z "$TAG" ]; then
  printf 'usage: %s <immutable-image-tag>\n' "$0" >&2
  exit 2
fi

if [ "$TAG" = "latest" ]; then
  printf 'refusing to deploy mutable tag "latest" to prod\n' >&2
  exit 2
fi

"$SCRIPT_DIR/check-context.sh"

IMAGE="$CONSIZE_IMAGE_REGISTRY/api:$TAG"
printf 'deploying prod API image: %s\n' "$IMAGE"
kubectl -n "$CONSIZE_NAMESPACE" set image deployment/consize-api "api=$IMAGE"
kubectl -n "$CONSIZE_NAMESPACE" rollout status deployment/consize-api --timeout=180s
kubectl -n "$CONSIZE_NAMESPACE" get pods -l app=consize-api -o wide
