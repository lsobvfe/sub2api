#!/usr/bin/env bash
# Sync official Sub2API, build the untouched upstream application plus the
# independent stream-hold proxy, then deploy the two-service topology.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
LOCAL_NATIVE="$ROOT/local-native"
BIN_DIR="$LOCAL_NATIVE/build"
RUNTIME="$LOCAL_NATIVE/runtime"
WORK="$RUNTIME/work"
SUB2API_ENV="$RUNTIME/sub2api.env"
PROXY_ENV="$RUNTIME/stream-hold-proxy.env"
SUB2API_BIN="$BIN_DIR/sub2api-source"
PROXY_BIN="$BIN_DIR/sub2api-stream-hold-proxy"
PROXY_ROOT="$LOCAL_NATIVE/extensions/stream-hold-proxy"
SUB2API_UNIT="$LOCAL_NATIVE/systemd/sub2api-source.service"
PROXY_UNIT="$LOCAL_NATIVE/systemd/sub2api-stream-hold-proxy.service"
SUB2API_SERVICE="${SUB2API_SYSTEMD_SERVICE:-sub2api-source.service}"
PROXY_SERVICE="${STREAM_HOLD_SYSTEMD_SERVICE:-sub2api-stream-hold-proxy.service}"
VERSION_FILE="$ROOT/backend/cmd/server/VERSION"
UPSTREAM_REMOTE="${SUB2API_UPSTREAM_REMOTE:-upstream}"
UPSTREAM_BRANCH="${SUB2API_UPSTREAM_BRANCH:-main}"
FETCH_ATTEMPTS="${SUB2API_FETCH_ATTEMPTS:-5}"
BUILD_ATTEMPTS="${SUB2API_BUILD_ATTEMPTS:-3}"
PNPM_VERSION="${SUB2API_PNPM_VERSION:-9.15.9}"

log() { printf '[sub2api] %s\n' "$*"; }
die() { printf '[sub2api] ERROR: %s\n' "$*" >&2; exit 1; }

read_source_version() {
  [[ -f "$VERSION_FILE" ]] || die "missing source version file: $VERSION_FILE"
  tr -d '[:space:]' <"$VERSION_FILE"
}

binary_version() {
  local bin="$1"
  [[ -x "$bin" ]] || return 1
  local line
  line="$("$bin" -version 2>&1 | head -n 1 || true)"
  if [[ "$line" =~ Sub2API[[:space:]]+([0-9][^[:space:]]*) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

read_env_var() {
  local file="$1"
  local key="$2"
  (
    set -a
    # shellcheck disable=SC1090
    . "$file"
    printf '%s' "${!key:-}"
  )
}

run_pnpm() {
  CI=true npx -y "pnpm@${PNPM_VERSION}" "$@"
}

fetch_upstream() {
  local attempt delay
  for attempt in $(seq 1 "$FETCH_ATTEMPTS"); do
    if git fetch "$UPSTREAM_REMOTE" --prune; then
      return 0
    fi
    delay=$((attempt * 2))
    log "fetch failed (attempt ${attempt}/${FETCH_ATTEMPTS}); retry in ${delay}s"
    sleep "$delay"
  done
  return 1
}

retry_build() {
  local description="$1"
  shift
  local attempt delay
  for attempt in $(seq 1 "$BUILD_ATTEMPTS"); do
    if "$@"; then
      return 0
    fi
    [[ "$attempt" -lt "$BUILD_ATTEMPTS" ]] || return 1
    delay=$((attempt * 2))
    log "${description} failed (attempt ${attempt}/${BUILD_ATTEMPTS}); retry in ${delay}s"
    sleep "$delay"
  done
}

assert_tracking_matches_remote_tip() {
  local merge_ref="$1"
  local remote_tip tracking_tip
  remote_tip="$(git ls-remote "$UPSTREAM_REMOTE" "refs/heads/${UPSTREAM_BRANCH}" | awk 'NR==1 {print $1}')"
  [[ -n "$remote_tip" ]] || die "remote tip is empty for ${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}"
  tracking_tip="$(git rev-parse --verify "$merge_ref")"
  [[ "$remote_tip" == "$tracking_tip" ]] ||
    die "$merge_ref is $tracking_tip but remote tip is $remote_tip"
}

wait_http() {
  local url="$1"
  local service="$2"
  local attempts="${3:-60}"
  local code
  for _ in $(seq 1 "$attempts"); do
    code="$(curl -sS -m 1 -o /dev/null -w '%{http_code}' "$url" || true)"
    if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then
      return 0
    fi
    if ! systemctl is-active --quiet "$service"; then
      sudo journalctl -u "$service" -n 80 --no-pager >&2 || true
      return 1
    fi
    sleep 0.5
  done
  return 1
}

listener_pids() {
  local port="$1"
  ss -ltnp "sport = :${port}" 2>/dev/null |
    grep -oE 'pid=[0-9]+' |
    cut -d= -f2 |
    sort -u || true
}

proxy_source_version() {
  find "$PROXY_ROOT" -type f \
    \( -name '*.go' ! -name '*_test.go' -o -name '*.html' -o -name 'go.mod' \) \
    -print0 |
    sort -z |
    xargs -0 sha256sum |
    sha256sum |
    cut -c1-12
}

for command in go npx git curl ss grep systemctl sudo strings sha256sum find sort xargs cmp; do
  command -v "$command" >/dev/null || die "$command not found"
done
[[ -f "$SUB2API_ENV" ]] || die "missing $SUB2API_ENV"
[[ -f "$PROXY_ENV" ]] || die "missing $PROXY_ENV"
[[ -f "$SUB2API_UNIT" ]] || die "missing $SUB2API_UNIT"
[[ -f "$PROXY_UNIT" ]] || die "missing $PROXY_UNIT"

cd "$ROOT"
git rev-parse --is-inside-work-tree >/dev/null || die "not a git repository: $ROOT"
log "git status (pre-commit)"
git status --short || true

# Snapshot all trackable changes before merging upstream. Ignored runtime
# secrets and build artifacts remain excluded by .gitignore.
git add -A
if ! git diff --cached --quiet; then
  if git diff --cached --name-only |
    grep -E '(^|/)sub2api\.env$|(^|/)config\.yaml$|\.dump$|\.env$' >/dev/null; then
    die "refusing to commit secrets/dumps; unstage and fix gitignore"
  fi
  BRANCH="$(git rev-parse --abbrev-ref HEAD)"
  MSG="chore: snapshot worktree before update-and-restart (${BRANCH})"
  log "committing whole-repo trackable changes: $MSG"
  git commit -m "$MSG"
else
  log "nothing to commit (clean trackable worktree)"
fi

mkdir -p "$BIN_DIR" "$WORK"

SUB2API_HOST="$(read_env_var "$SUB2API_ENV" SERVER_HOST)"
SUB2API_PORT="$(read_env_var "$SUB2API_ENV" SERVER_PORT)"
PROXY_LISTEN="$(read_env_var "$PROXY_ENV" STREAM_HOLD_LISTEN_ADDR)"
PROXY_UPSTREAM="$(read_env_var "$PROXY_ENV" STREAM_HOLD_UPSTREAM_URL)"
[[ -n "$SUB2API_HOST" && -n "$SUB2API_PORT" ]] || die "SERVER_HOST and SERVER_PORT are required"
[[ "$SUB2API_HOST" == "127.0.0.1" || "$SUB2API_HOST" == "::1" ]] ||
  die "Sub2API must bind only to loopback behind the proxy (SERVER_HOST=$SUB2API_HOST)"
[[ "$PROXY_LISTEN" =~ :([0-9]+)$ ]] || die "invalid STREAM_HOLD_LISTEN_ADDR=$PROXY_LISTEN"
PUBLIC_PORT="${BASH_REMATCH[1]}"
EXPECTED_UPSTREAM="http://${SUB2API_HOST}:${SUB2API_PORT}"
[[ "$PROXY_UPSTREAM" == "$EXPECTED_UPSTREAM" ]] ||
  die "STREAM_HOLD_UPSTREAM_URL must equal $EXPECTED_UPSTREAM"
[[ "$PUBLIC_PORT" != "$SUB2API_PORT" ]] || die "proxy and Sub2API ports must differ"

SOURCE_VERSION="$(read_source_version)"
log "root=$ROOT source_version=$SOURCE_VERSION internal=:${SUB2API_PORT} public=:${PUBLIC_PORT}"

log "fetch upstream"
fetch_upstream || die "fetch $UPSTREAM_REMOTE failed after ${FETCH_ATTEMPTS} attempts"

CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "$CURRENT_BRANCH" != "HEAD" ]] || die "detached HEAD"
TRACK_REF="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
if [[ "$TRACK_REF" == "$UPSTREAM_REMOTE"/* ]]; then
  MERGE_REF="$TRACK_REF"
else
  MERGE_REF="$UPSTREAM_REMOTE/$UPSTREAM_BRANCH"
fi
git rev-parse --verify "$MERGE_REF" >/dev/null || die "missing ref $MERGE_REF"
assert_tracking_matches_remote_tip "$MERGE_REF"

AHEAD="$(git rev-list --count "${MERGE_REF}..HEAD")"
BEHIND="$(git rev-list --count "HEAD..${MERGE_REF}")"
log "git HEAD=$(git rev-parse --short=12 HEAD) ${MERGE_REF}=$(git rev-parse --short=12 "$MERGE_REF") ahead=${AHEAD} behind=${BEHIND}"
if [[ "$BEHIND" -gt 0 ]]; then
  log "merge ${BEHIND} upstream commit(s)"
  git merge --no-edit "$MERGE_REF" ||
    die "merge conflict with $MERGE_REF; resolve it, commit, then rerun"
fi
[[ -z "$(git status --porcelain)" ]] || die "worktree became dirty after upstream merge"

SOURCE_VERSION="$(read_source_version)"
log "building frontend with pnpm ${PNPM_VERSION}"
run_pnpm --dir "$ROOT/frontend" --ignore-workspace install --frozen-lockfile
run_pnpm --dir "$ROOT/frontend" --ignore-workspace run build
WEB_DIST="$ROOT/backend/internal/web/dist"
[[ -f "$WEB_DIST/index.html" ]] || die "frontend dist missing $WEB_DIST/index.html"

log "building official Sub2API source (-tags embed)"
VERSION="$(cd "$ROOT/backend" && ./scripts/resolve-version.sh)"
[[ "$VERSION" == "$SOURCE_VERSION" ]] ||
  die "resolved version $VERSION does not match source version $SOURCE_VERSION"
SUB2API_TMP="$BIN_DIR/sub2api-source.new"
build_sub2api() (
  cd "$ROOT/backend"
  GOTOOLCHAIN=auto CGO_ENABLED=0 go build \
    -tags embed \
    -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o "$SUB2API_TMP" \
    ./cmd/server
)
retry_build "Sub2API build" build_sub2api ||
  die "Sub2API build failed after ${BUILD_ATTEMPTS} attempts"
if strings "$SUB2API_TMP" | grep -F 'Frontend not embedded' >/dev/null; then
  rm -f "$SUB2API_TMP"
  die "Sub2API binary does not contain the embedded frontend"
fi
BIN_VERSION="$(binary_version "$SUB2API_TMP")" || die "cannot read Sub2API binary version"
[[ "$BIN_VERSION" == "$SOURCE_VERSION" ]] ||
  die "Sub2API binary version $BIN_VERSION does not match source $SOURCE_VERSION"

PROXY_VERSION="$(proxy_source_version)"
PROXY_TMP="$BIN_DIR/sub2api-stream-hold-proxy.new"
log "building independent stream-hold proxy version=${PROXY_VERSION}"
build_proxy() (
  cd "$PROXY_ROOT"
  CGO_ENABLED=0 go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w -buildid= -X main.version=${PROXY_VERSION}" \
    -o "$PROXY_TMP" \
    ./cmd/stream-hold-proxy
)
retry_build "stream-hold proxy build" build_proxy ||
  die "stream-hold proxy build failed after ${BUILD_ATTEMPTS} attempts"
"$PROXY_TMP" -version | grep -F "$PROXY_VERSION" >/dev/null ||
  die "cannot verify stream-hold proxy version"

PROXY_CHANGED=1
if [[ -x "$PROXY_BIN" ]] && cmp -s "$PROXY_BIN" "$PROXY_TMP"; then
  PROXY_CHANGED=0
  rm -f "$PROXY_TMP"
fi

PROXY_UNIT_CHANGED=0
if [[ ! -f "/etc/systemd/system/$PROXY_SERVICE" ]] ||
  ! cmp -s "$PROXY_UNIT" "/etc/systemd/system/$PROXY_SERVICE"; then
  PROXY_UNIT_CHANGED=1
fi

if [[ -f "$SUB2API_BIN" ]]; then
  cp -af "$SUB2API_BIN" "$BIN_DIR/sub2api-source.previous"
fi
mv -f "$SUB2API_TMP" "$SUB2API_BIN"
chmod +x "$SUB2API_BIN"
if [[ "$PROXY_CHANGED" -eq 1 ]]; then
  mv -f "$PROXY_TMP" "$PROXY_BIN"
  chmod +x "$PROXY_BIN"
fi

sudo install -m 0644 "$SUB2API_UNIT" "/etc/systemd/system/$SUB2API_SERVICE"
sudo install -m 0644 "$PROXY_UNIT" "/etc/systemd/system/$PROXY_SERVICE"
sudo systemctl daemon-reload
sudo systemctl enable "$SUB2API_SERVICE" "$PROXY_SERVICE" >/dev/null

log "restarting official Sub2API on internal port ${SUB2API_PORT}"
sudo systemctl restart "$SUB2API_SERVICE"
wait_http "http://127.0.0.1:${SUB2API_PORT}/health" "$SUB2API_SERVICE" ||
  die "Sub2API failed its internal health check"

if [[ "$PROXY_CHANGED" -eq 1 || "$PROXY_UNIT_CHANGED" -eq 1 ]] ||
  ! systemctl is-active --quiet "$PROXY_SERVICE"; then
  log "starting/restarting stream-hold proxy on public port ${PUBLIC_PORT}"
  sudo systemctl restart "$PROXY_SERVICE"
else
  log "stream-hold proxy binary unchanged; keeping the existing process and active client streams"
fi

wait_http "http://127.0.0.1:${PUBLIC_PORT}/_stream-hold/health" "$PROXY_SERVICE" ||
  die "stream-hold proxy failed its health check"
wait_http "http://127.0.0.1:${PUBLIC_PORT}/health" "$PROXY_SERVICE" ||
  die "public Sub2API health check through the proxy failed"

ROOT_CODE="$(curl -sS -m 5 -o /tmp/sub2api-root.html -w '%{http_code}' "http://127.0.0.1:${PUBLIC_PORT}/" || true)"
[[ "$ROOT_CODE" == "200" ]] || die "public frontend returned HTTP $ROOT_CODE"
grep -Ei '<!doctype html>|<title>' /tmp/sub2api-root.html >/dev/null ||
  die "public frontend is not serving HTML"

SUB2API_PID="$(systemctl show "$SUB2API_SERVICE" -p MainPID --value)"
PROXY_PID="$(systemctl show "$PROXY_SERVICE" -p MainPID --value)"
mapfile -t INTERNAL_PIDS < <(listener_pids "$SUB2API_PORT")
mapfile -t PUBLIC_PIDS < <(listener_pids "$PUBLIC_PORT")
[[ ${#INTERNAL_PIDS[@]} -eq 1 && "${INTERNAL_PIDS[0]}" == "$SUB2API_PID" ]] ||
  die "internal port ${SUB2API_PORT} is not owned solely by $SUB2API_SERVICE pid=$SUB2API_PID"
[[ ${#PUBLIC_PIDS[@]} -eq 1 && "${PUBLIC_PIDS[0]}" == "$PROXY_PID" ]] ||
  die "public port ${PUBLIC_PORT} is not owned solely by $PROXY_SERVICE pid=$PROXY_PID"

log "OK source=${SOURCE_VERSION} sub2api_pid=${SUB2API_PID} proxy_pid=${PROXY_PID} proxy_version=${PROXY_VERSION} public=:${PUBLIC_PORT} internal=:${SUB2API_PORT}"
