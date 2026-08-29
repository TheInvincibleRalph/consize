#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"

if [ ! -f "$ENV_FILE" ]; then
  ENV_FILE="$SCRIPT_DIR/.env.example"
fi

set -a
. "$ENV_FILE"
set +a

"$SCRIPT_DIR/migrate.sh"

cd "$REPO_ROOT/engine"
exec go run ./cmd/api
