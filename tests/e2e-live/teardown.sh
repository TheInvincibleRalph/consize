#!/usr/bin/env bash
# Reverse the live E2E: remove the Consize stack, the sandbox, the
# canary, and the pushed images. boutique keeps the values the E2E
# applied unless --restore-boutique is passed (then frontend returns
# to its pre-E2E requests, saved by run.sh at apply time).
set -euo pipefail

PROJECT="${CONSIZE_E2E_PROJECT:-devops-portfolio-prod}"
REGION="${CONSIZE_E2E_REGION:-us-central1}"
IMG="${CONSIZE_E2E_IMG:-us-central1-docker.pkg.dev/devops-portfolio-prod/consize}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/out"

log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m✔ %s\033[0m\n' "$*"; }

if [ "${1:-}" = "--restore-boutique" ] && [ -f "$OUT/track1-orig.json" ]; then
  log "restoring boutique/frontend to pre-E2E resources"
  orig="$(cat "$OUT/track1-orig.json")"
  kubectl -n boutique patch deploy frontend -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"server\",\"resources\":$orig}]}}}}"
  ok "frontend restored"
fi

log "removing namespaces (consize-system includes Postgres, roles, SAs)"
kubectl delete ns consize-sandbox --ignore-not-found --wait=true
kubectl delete ns consize-system --ignore-not-found --wait=true
ok "namespaces gone"

log "removing images"
for b in collector analyze api verify migrate; do
  gcloud artifacts docker images delete "$IMG/$b:e2e" --delete-tags --quiet >/dev/null 2>&1 || true
done
ok "images gone"

log "teardown complete — cluster back to its pre-test state"
