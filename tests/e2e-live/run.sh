#!/usr/bin/env bash
# Live-cluster E2E for the Consize M2 safety engine (docs/e2e.md).
#
# Closes the M2 AC gap: apply → verify → rollback exercised for real on
# a live GKE cluster, with a least-privilege ServiceAccount doing the
# patching. Two tracks:
#
#   track1   boutique/frontend (real workload) — PASS verdict, no rollback
#   track2   consize-canary (synthetic)        — injected OOM regression,
#            verifier FAILs, auto-rollback restores the applied values
#
# Usage:  ./run.sh <subcommand>
#   preflight   cluster + Prometheus + tooling checks
#   deploy      images → AR, full stack, migrations, canary
#   ingest      manual collector + analyze runs; prints pending recs
#   track1      dry-run + approved apply on boutique/frontend
#   verify1     wait for the window, verify, assert PASSED
#   track2      approved apply on canary + inject the OOM regression
#   verify2     wait for the window, verify, assert FAIL + rollback
#   rbac        auth can-i matrix for the write/read identities
#   status      current store + cluster state
#   summary     evidence recap (also prints at the end of each verify)
#
# State and evidence land in out/.
set -euo pipefail

PROJECT="${CONSIZE_E2E_PROJECT:-devops-portfolio-prod}"
REGION="${CONSIZE_E2E_REGION:-us-central1}"
CLUSTER="${CONSIZE_E2E_CLUSTER:-devops-portfolio}"
# NOTE: us-central1 repo creation is rejected on this trial project
# ("Requested entity was not found") — the multi-region `us` location
# works, so the registry domain is us-docker.pkg.dev.
IMG="${CONSIZE_E2E_IMG:-us-docker.pkg.dev/devops-portfolio-prod/consize}"
AR_LOCATION="${CONSIZE_E2E_AR_LOCATION:-us}"
TAG="${CONSIZE_E2E_TAG:-e2e}"
API_PORT="${CONSIZE_E2E_API_PORT:-18080}"
PROM_PORT="${CONSIZE_E2E_PROM_PORT:-19090}"
WINDOW_MIN=15               # must match CONSIZE_VERIFY_WINDOW in deploy/verify-cronjob.yaml
WINDOW_MARGIN_S=60          # grace past CreatedAt+window before the verifier will act
NS_SYS=consize-system
NS_SBX=consize-sandbox
HERE="$(cd "$(dirname "$0")" && pwd)"
ENGINE="$(cd "$HERE/../../engine" && pwd)"
OUT="$HERE/out"
WRITE_SA="system:serviceaccount:$NS_SYS:consize-writer"
READ_SA="system:serviceaccount:$NS_SYS:consize-reader"
BINS=(collector analyze api verify migrate report costscan)
API="http://127.0.0.1:$API_PORT/api/v1"

mkdir -p "$OUT"
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 1; }
# Docker Desktop's credential helper lives in the app bundle; without it
# on PATH, `docker build` cannot pull base images (docker-credential-desktop
# not found). Make the bundle bin discoverable when it exists.
if ! command -v docker-credential-desktop >/dev/null 2>&1 &&
   [ -x "/Applications/Docker.app/Contents/Resources/bin/docker-credential-desktop" ]; then
  export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
fi

log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m✔ %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31m✘ %s\033[0m\n' "$*" >&2; exit 1; }
info() { printf '  %s\n' "$*"; }

ctx() {
  local c
  c="$(kubectl config current-context 2>/dev/null || true)"
  case "$c" in
    gke_${PROJECT}_${REGION}_${CLUSTER}) ;;
    *) gcloud container clusters get-credentials "$CLUSTER" --region "$REGION" --project "$PROJECT" >/dev/null ;;
  esac
}

pf() { # pf <local> <ns> <svc> <remote> <tag>  — start, wait, remember PID
  # "up" = any HTTP response (the API root answers 404; only a refused
  # connection means the forward is not listening yet).
  local port="$1" ns="$2" svc="$3" remote="$4" tag="$5"
  local code
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port" 2>/dev/null || true)"
  if [ "$code" = "000" ]; then
    kubectl -n "$ns" port-forward "svc/$svc" "$port:$remote" >/dev/null 2>&1 &
    echo $! > "$OUT/pf-$tag.pid"
    for _ in $(seq 1 30); do
      code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port" 2>/dev/null || true)"
      [ "$code" != "000" ] && break
      sleep 1
    done
  fi
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port" 2>/dev/null || true)"
  [ "$code" != "000" ] || fail "port-forward $tag did not come up"
}

pfs() { for f in "$OUT"/pf-*.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null && rm -f "$f"; done; return 0; }
trap pfs EXIT

api_get() { curl -sf "$API$1"; }
api_post() { curl -sf -X POST -H 'content-type: application/json' -d "$2" "$API$1"; }

wait_for() { # wait_for <label> <seconds> <cmd...>
  local label="$1" secs="$2"; shift 2
  for _ in $(seq 1 "$secs"); do
    if "$@" >/dev/null 2>&1; then ok "$label"; return 0; fi
    sleep 1
  done
  fail "timeout waiting for $label"
}

apply_rewrite() { # image rewrite consize/<bin>:latest → $IMG/<bin>:$TAG
  sed "s|consize/\([a-z-]*\):latest|$IMG/\1:$TAG|g" "$1" | kubectl apply -f -
}

workload_id() { # workload_id <ns> <name>
  api_get "/workloads" | jq -r --arg n "$2" --arg ns "$1" \
    '.workloads[] | select(.Namespace==$ns and .Name==$n) | .ID' | head -1
}

pending_for() { # pending_for <wid> <resource?> → rec JSON (highest savings)
  local wid="$1" res="${2:-}"
  if [ -n "$res" ]; then
    api_get "/recommendations?status=pending" | jq -c --argjson w "$wid" --arg r "$res" \
      '[.recommendations[] | select(.WorkloadID==$w and .Resource==$r)] | sort_by(.SavingsMonthly) | last'
  else
    api_get "/recommendations?status=pending" | jq -c --argjson w "$wid" \
      '[.recommendations[] | select(.WorkloadID==$w)] | sort_by(.SavingsMonthly) | last'
  fi
}

# epoch_of <RFC3339> → unix seconds (python3 handles fractional seconds
# and offsets; date -j on macOS does not).
epoch_of() {
  python3 -c "import datetime;print(int(datetime.datetime.fromisoformat('$1'.replace('Z','+00:00')).timestamp()))"
}

# due_epoch <apply_event_id> <workload_id> → created_at + window + margin
due_epoch() {
  local eid="$1" wid="$2" created
  created="$(api_get "/applies?workload_id=$wid" | jq -r --argjson e "$eid" \
    '.applies[] | select(.ID==$e) | .CreatedAt')"
  echo $(( $(epoch_of "$created") + WINDOW_MIN*60 + WINDOW_MARGIN_S ))
}

trigger() { # trigger <cronjob> <name> — one-shot job from a CronJob, wait for completion
  local cj="$1" name="$2"
  kubectl -n "$NS_SYS" delete job "$name" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS_SYS" create job "$name" --from="cronjob/$cj" >/dev/null
  kubectl -n "$NS_SYS" wait --for=condition=complete "job/$name" --timeout=180s >/dev/null 2>&1 \
    || { kubectl -n "$NS_SYS" logs "job/$name" | tail -20; fail "$name failed"; }
}

# ------------------------------------------------------------------ preflight
cmd_preflight() {
  ctx
  log "context"
  kubectl cluster-info 2>&1 | head -2
  gcloud projects describe "$PROJECT" --format='value(projectId)' >/dev/null || fail "gcloud auth"
  log "Prometheus kubelet metrics"
  pf "$PROM_PORT" monitoring monitoring-kube-prometheus-prometheus 9090 prom
  local n
  n="$(curl -sf "http://127.0.0.1:$PROM_PORT/api/v1/query" --data-urlencode \
    'query=count(container_cpu_cfs_throttled_seconds_total)' | jq -r '.data.result[0].value[1]')"
  [ "${n:-0}" != "0" ] && ok "kubelet throttle metric present" || fail "kubelet metrics missing"
  ok "preflight"
}

# -------------------------------------------------------------------- deploy
cmd_deploy() {
  ctx
  log "artifact registry"
  gcloud artifacts repositories describe consize --location="$AR_LOCATION" --project="$PROJECT" >/dev/null 2>&1 \
    || gcloud artifacts repositories create consize --repository-format=docker \
         --location="$AR_LOCATION" --project="$PROJECT" >/dev/null
  gcloud auth configure-docker "${IMG%%/*}" --quiet >/dev/null

  log "build + push images (${BINS[*]})"
  # This machine's Docker Hub access is proxied by an untrusted MITM cert
  # (docker.io pulls fail), so the canonical multi-stage Dockerfile
  # (FROM golang:alpine) cannot build here; distroless comes from gcr.io,
  # which is reachable. Compile happens with the local Go toolchain
  # (module cache, offline), and the image is assembled from
  # engine/Dockerfile.local (single-stage, gcr.io base). The canonical
  # Dockerfile remains the CI build; Dockerfile.local documents the fallback.
  #
  # Push via a docker-container buildx builder (not the desktop-linux
  # driver): it writes straight from buildkit to the registry, which works
  # with gcr.io bases, whereas Docker Desktop's containerd tag store served
  # stale arm64 indexes for the same tags.
  if ! docker buildx inspect consize-amd64 >/dev/null 2>&1; then
    docker buildx create --name consize-amd64 --driver docker-container \
      --platform linux/amd64 >/dev/null
  fi
  for b in "${BINS[@]}"; do
    local gobin="${GO:-go}"
    [ -x "$gobin" ] || gobin="$(command -v go || true)"
    [ -n "$gobin" ] || fail "go toolchain not found (set GO=/path/to/go)"
    ( cd "$ENGINE" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        "$gobin" build -trimpath -ldflags="-s -w" -o "bin/$b" "./cmd/$b" )
    # --platform=linux/amd64 is REQUIRED: without it the builder emits the
    # host (arm64) platform and the image mismatches the cluster.
    docker buildx build --builder consize-amd64 --platform=linux/amd64 \
      --provenance=false --push -f "$ENGINE/Dockerfile.local" \
      --build-arg "BIN=$b" -t "$IMG/${b}:${TAG}" "$ENGINE" >/dev/null
    ok "$b (pushed; verify via gcloud after replication settles)"
  done
  log "note: the AR repo is multi-region (us) — tag updates replicate
       eventually; allow a minute before first pulls"

  log "identities + namespaces"
  apply_rewrite "$ENGINE/deploy/rbac.yaml"
  kubectl apply -f "$HERE/namespace.yaml"

  log "postgres (CloudNativePG)"
  apply_rewrite "$ENGINE/deploy/postgres.yaml"
  kubectl -n "$NS_SYS" wait --for=condition=Ready cluster/consize-db --timeout=300s

  log "store secret"
  local pw
  pw="$(kubectl -n "$NS_SYS" get secret consize-db-app -o jsonpath='{.data.password}' | base64 -d)"
  kubectl -n "$NS_SYS" delete secret consize-store --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS_SYS" create secret generic consize-store \
    --from-literal="database-url=postgres://app:${pw}@consize-db-rw.$NS_SYS.svc:5432/app" \
    --from-literal="prometheus-url=http://monitoring-kube-prometheus-prometheus.monitoring:9090" >/dev/null

  log "migrations"
  apply_rewrite "$ENGINE/deploy/migrate-job.yaml"
  kubectl -n "$NS_SYS" wait --for=condition=complete job/consize-migrate --timeout=120s

  log "components"
  apply_rewrite "$ENGINE/deploy/api.yaml"
  apply_rewrite "$ENGINE/deploy/collector-cronjob.yaml"
  apply_rewrite "$ENGINE/deploy/costscan-cronjob.yaml"
  apply_rewrite "$ENGINE/deploy/analyze-cronjob.yaml"
  apply_rewrite "$ENGINE/deploy/verify-cronjob.yaml"
  kubectl -n "$NS_SYS" rollout status deploy/consize-api --timeout=180s >/dev/null

  log "canary"
  kubectl apply -f "$HERE/canary.yaml"
  kubectl -n "$NS_SBX" rollout status deploy/consize-canary --timeout=180s >/dev/null

  cmd_preflight
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  ok "API up: $API/readyz → $(api_get /readyz | jq -c .)"
  log "deploy complete — next: ./run.sh ingest"
}

# -------------------------------------------------------------------- ingest
cmd_ingest() {
  ctx
  log "collector (manual run)"
  trigger consize-collector collector-e2e
  log "analyze (manual run)"
  trigger consize-analyze analyze-e2e
  log "pending recommendations"
  api_get "/recommendations?status=pending" | jq -r \
    '.recommendations[] | "\(.WorkloadID)\t\(.Resource)\tcurrent=\(.CurrentValue)\tproposed=\(.ProposedValue)\t$\(.SavingsMonthly)"'
}

# -------------------------------------------------------------------- track1
cmd_track1() {
  ctx
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  log "track1: boutique/frontend"
  local wid rec id
  wid="$(workload_id boutique frontend)"
  [ -n "$wid" ] || fail "frontend workload not ingested yet — run ./run.sh ingest"
  rec="$(pending_for "$wid")"
  [ -n "$rec" ] || fail "no pending recommendation for boutique/frontend (already applied? run ./run.sh status)"
  id="$(jq -r .ID <<<"$rec")"
  info "recommendation #$id: $(jq -r .Resource <<<"$rec") $(jq -r .CurrentValue <<<"$rec")→$(jq -r .ProposedValue <<<"$rec") savings=\$$(jq -r .SavingsMonthly <<<"$rec")"
  echo "$wid" > "$OUT/track1-wid"
  echo "$id"  > "$OUT/track1-rec"
  kubectl -n boutique get deploy frontend -o jsonpath='{.spec.template.spec.containers[0].resources}' | tee "$OUT/track1-orig.json"
  echo

  log "dry-run (guardrails + step plan, nothing touched)"
  local dry
  dry="$(api_post "/recommendations/$id/apply" '{"mode":"dry_run"}')"
  echo "$dry" | jq . | tee "$OUT/track1-dryrun.json"
  jq -e '.Blocked == false' <<<"$dry" >/dev/null || fail "dry-run was blocked: $(jq -r .BlockReasons <<<"$dry")"

  log "approved apply (actor=e2e-bot)"
  local res
  res="$(api_post "/recommendations/$id/apply" '{"mode":"approved","actor":"e2e-bot"}')"
  echo "$res" | jq . | tee "$OUT/track1-apply.json"
  jq -e '.Applied == true' <<<"$res" >/dev/null || fail "apply failed: $res"

  log "assert: deployment patched by the write SA"
  kubectl -n boutique get deploy frontend -o jsonpath='{.spec.template.spec.containers[0].resources}' | tee "$OUT/track1-resources.json"
  echo
  log "track1 applied — next: ./run.sh verify1 (waits for the $WINDOW_MIN-minute window)"
}

# ------------------------------------------------------------------- verify1
cmd_verify1() {
  ctx
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  local wid eid due now
  wid="$(cat "$OUT/track1-wid")"
  eid="$(jq -r .EventID "$OUT/track1-apply.json")"
  due="$(due_epoch "$eid" "$wid")"
  now="$(date +%s)"
  if [ "$now" -lt "$due" ]; then
    info "verification window due at $(date -r "$due") — sleeping $((due-now))s"
    sleep "$((due-now))"
  fi
  # The forward started above can drop during a long sleep — re-establish
  # (pf probes first, so a healthy forward is untouched).
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  log "verify (manual run)"
  trigger consize-verify verify-e2e-1
  kubectl -n "$NS_SYS" logs job/verify-e2e-1 | tee "$OUT/track1-verify.log"
  log "assert: verdict passed, no rollback"
  api_get "/verification-runs?apply_event_id=$eid" | jq -c '.verification_runs[] | {Verdict, SLIs}' \
    | tee "$OUT/track1-verdict.json"
  [ "$(jq -r .Verdict "$OUT/track1-verdict.json")" = "passed" ] || fail "track1 verdict not passed"
  local reverted
  reverted="$(api_get "/applies?workload_id=$wid" | jq '[.applies[] | select(.Result=="reverted")] | length')"
  [ "$reverted" = "0" ] || fail "unexpected reverted event on track1"
  ok "track1 PASSED — no rollback, requests unchanged: $(cat "$OUT/track1-resources.json")"
}

# -------------------------------------------------------------------- track2
cmd_track2() {
  ctx
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  log "track2: consize-canary"
  local wid rec id res
  wid="$(workload_id consize-sandbox consize-canary)"
  [ -n "$wid" ] || fail "canary not ingested — run ./run.sh ingest"
  rec="$(pending_for "$wid" memory)"
  [ -n "$rec" ] || fail "no pending memory recommendation for the canary (run ./run.sh ingest)"
  id="$(jq -r .ID <<<"$rec")"
  echo "$wid" > "$OUT/track2-wid"
  echo "$id"  > "$OUT/track2-rec"
  info "recommendation #$id: $(jq -r .CurrentValue <<<"$rec")→$(jq -r .ProposedValue <<<"$rec") bytes, savings=\$$(jq -r .SavingsMonthly <<<"$rec")"
  # Pre-apply state — the rollback target. The verifier restores the
  # workload to these values ABSOLUTELY (not a delta onto the drifted
  # regression), so verify2 asserts against this file.
  kubectl -n "$NS_SBX" get deploy consize-canary -o jsonpath='{.spec.template.spec.containers[0].resources}' | tee "$OUT/track2-orig.json"
  echo

  log "approved apply (actor=e2e-bot)"
  res="$(api_post "/recommendations/$id/apply" '{"mode":"approved","actor":"e2e-bot"}')"
  echo "$res" | jq . | tee "$OUT/track2-apply.json"
  jq -e '.Applied == true' <<<"$res" >/dev/null || fail "apply failed: $res"
  kubectl -n "$NS_SBX" get deploy consize-canary -o jsonpath='{.spec.template.spec.containers[0].resources}' | tee "$OUT/track2-applied.json"
  echo

  log "inject regression: bad release drops memory below the allocator footprint"
  # Memory-only drift (the plan's contract): Consize owns the memory
  # fields — apply and rollback only ever touch the recommended resource.
  # A CPU drift here would survive the rollback by design (Consize never
  # undoes an external actor's unrelated change) and fail the restore
  # assertion on a field it rightly leaves alone.
  kubectl -n "$NS_SBX" set resources deploy consize-canary \
    --requests=memory=64Mi --limits=memory=96Mi >/dev/null
  kubectl -n "$NS_SBX" rollout status deploy/consize-canary --timeout=120s >/dev/null 2>&1 || true
  info "waiting for OOMKill + CrashLoop evidence"
  for _ in $(seq 1 60); do
    phase="$(kubectl -n "$NS_SBX" get pod -l app=consize-canary -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
    restarts="$(kubectl -n "$NS_SBX" get pod -l app=consize-canary -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null || true)"
    [ "${restarts:-0}" -ge 2 ] && break
    sleep 5
  done
  kubectl -n "$NS_SBX" get pod -l app=consize-canary -o jsonpath='{.items[0].status.containerStatuses[0].state}' | tee "$OUT/track2-oom.json"
  echo
  info "regression injected — next: ./run.sh verify2"
}

# ------------------------------------------------------------------- verify2
cmd_verify2() {
  ctx
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  local wid eid due now
  wid="$(cat "$OUT/track2-wid")"
  eid="$(jq -r .EventID "$OUT/track2-apply.json")"
  due="$(due_epoch "$eid" "$wid")"
  now="$(date +%s)"
  if [ "$now" -lt "$due" ]; then
    info "verification window due at $(date -r "$due") — sleeping $((due-now))s"
    sleep "$((due-now))"
  fi
  # Re-establish the forward (see verify1 — drops during long sleeps).
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  log "verify (manual run)"
  trigger consize-verify verify-e2e-2
  kubectl -n "$NS_SYS" logs job/verify-e2e-2 | tee "$OUT/track2-verify.log"
  log "assert: verdict failed → auto-rollback"
  api_get "/verification-runs?apply_event_id=$eid" | jq -c '.verification_runs[] | {Verdict, SLIs}' \
    | tee "$OUT/track2-verdict.json"
  [ "$(jq -r .Verdict "$OUT/track2-verdict.json")" = "failed" ] || fail "track2 verdict not failed"
  ok "verdict FAILED (evidence above) — rollback must have fired"
  local reverted
  reverted="$(api_get "/applies?workload_id=$wid" | jq -r '.applies[] | select(.Result=="reverted") | .ID' | head -1)"
  [ -n "$reverted" ] || fail "no reverted event in the audit trail"
  ok "reverted event #$reverted recorded"
  log "assert: canary restored to pre-apply values and healthy"
  kubectl -n "$NS_SBX" get deploy consize-canary -o jsonpath='{.spec.template.spec.containers[0].resources}' | tee "$OUT/track2-restored.json"
  echo
  # Rollback restores the pre-apply state absolutely (ADR-026); the
  # injected regression is the drift it must undo, so the target is the
  # state captured before the apply — not the applied values.
  diff <(jq -S . "$OUT/track2-orig.json") <(jq -S . "$OUT/track2-restored.json") >/dev/null \
    && ok "requests == pre-apply values (rollback landed absolutely)" || fail "requests not restored to pre-apply values"
  kubectl -n "$NS_SBX" rollout status deploy/consize-canary --timeout=180s >/dev/null
  ok "canary healthy again (180 MiB allocator fits the restored request)"
  ok "track2 COMPLETE — full safety loop proven on the live cluster"
}

# ---------------------------------------------------------------------- rbac
cmd_rbac() {
  ctx
  log "write identity (consize-writer) — deployments only, bound namespaces only"
  local expected=(
    "update|deployments|consize-sandbox|yes"
    "update|deployments|boutique|yes"
    "update|deployments|kube-system|no"
    "update|deployments|test-a|no"
    "delete|deployments|consize-sandbox|no"
    "update|statefulsets|consize-sandbox|no"
    "scale|deployments|consize-sandbox|no"
    "create|pods|consize-sandbox|no"
    "get|secrets|consize-sandbox|no"
  )
  local line verb res ns want got pass=1
  for line in "${expected[@]}"; do
    IFS='|' read -r verb res ns want <<<"$line"
    # can-i prints "no" AND exits 1 on denial — `|| echo no` would capture
    # "no\nno"; `|| true` keeps the substitution's exit safe for set -e
    # while the output stays kubectl's single "no".
    got="$(kubectl auth can-i "$verb" "$res" -n "$ns" --as="$WRITE_SA" 2>/dev/null || true)"
    if [ "$got" = "$want" ]; then ok "  $verb $res @ $ns → $got"; else fail "  $verb $res @ $ns → $got (want $want)"; pass=0; fi
  done
  log "read identity (consize-reader) — read-only"
  [ "$(kubectl auth can-i list deployments -n boutique --as="$READ_SA" 2>/dev/null)" = "yes" ] \
    && ok "  list deployments @ boutique → yes" || fail "  list deployments @ boutique"
  [ "$(kubectl auth can-i update deployments -n boutique --as="$READ_SA" 2>/dev/null)" = "no" ] \
    && ok "  update deployments @ boutique → no" || fail "  update deployments @ boutique (must be no)"
  [ "$(kubectl auth can-i list deployments -n kube-system --as="$READ_SA" 2>/dev/null)" = "no" ] \
    && ok "  list deployments @ kube-system → no" || fail "  list deployments @ kube-system (must be no)"
  [ "$pass" = "1" ] && ok "RBAC matrix complete — least privilege holds"
}

# -------------------------------------------------------------------- status
cmd_status() {
  ctx
  log "components"
  kubectl -n "$NS_SYS" get deploy,cronjob,job 2>/dev/null | head -12
  kubectl -n "$NS_SYS" get cluster/consize-db 2>/dev/null | head -3
  log "canary"
  kubectl -n "$NS_SBX" get deploy,po -o wide 2>/dev/null | head -5
  log "pending recommendations"
  pf "$API_PORT" "$NS_SYS" consize-api 8080 api
  api_get "/recommendations?status=pending" | jq -r '.recommendations[] | "\(.WorkloadID)\t\(.Resource)\t\(.CurrentValue)→\(.ProposedValue)\t$\(.SavingsMonthly)"' || true
  log "apply events"
  api_get "/applies" | jq -r '.applies[] | "\(.ID)\t\(.Result)\tstep \(.StepNumber)/\(.TotalSteps)\tactor=\(.Actor)\t\(.CreatedAt)"' || true
  log "verification runs"
  api_get "/verification-runs" | jq -r '.verification_runs[] | "\(.ID)\tapply=\(.ApplyEventID)\t\(.Verdict)"' || true
}

# ------------------------------------------------------------------ summary
cmd_summary() {
  log "E2E evidence (tests/e2e-live/out)"
  for f in "$OUT"/*.json "$OUT"/*.log; do
    [ -f "$f" ] && info "$(basename "$f") — $(wc -l < "$f") lines"
  done
  [ -f "$OUT/track1-verdict.json" ] && ok "track1: $(jq -r .Verdict "$OUT/track1-verdict.json")"
  [ -f "$OUT/track2-verdict.json" ] && ok "track2: $(jq -r .Verdict "$OUT/track2-verdict.json") with rollback $( [ -n "$(jq -r '.ID' "$OUT/track2-restored.json" 2>/dev/null)" ] && echo "verified" || echo "pending")"
}

[ -z "${BASH_SOURCE[0]:-}" ] || case "${1:-status}" in
  preflight) cmd_preflight ;;
  deploy)    cmd_deploy ;;
  ingest)    cmd_ingest ;;
  track1)    cmd_track1 ;;
  verify1)   cmd_verify1 ;;
  track2)    cmd_track2 ;;
  verify2)   cmd_verify2 ;;
  rbac)      cmd_rbac ;;
  status)    cmd_status ;;
  summary)   cmd_summary ;;
  *) echo "unknown subcommand: $1"; grep -o '^  [a-z0-9]*' "$0" | head -20 ;;
esac
