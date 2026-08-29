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

"$SCRIPT_DIR/check-context.sh"

printf 'forwarding prod API: http://127.0.0.1:%s -> svc/%s:8080\n' \
  "$CONSIZE_API_LOCAL_PORT" "$CONSIZE_API_SERVICE"
exec kubectl -n "$CONSIZE_NAMESPACE" port-forward \
  "svc/$CONSIZE_API_SERVICE" "$CONSIZE_API_LOCAL_PORT:8080"
