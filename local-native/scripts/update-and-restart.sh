#!/usr/bin/env bash
# 1) git 提交**整个仓库**当前可跟踪变更（gitignore 排除密钥/构建物）
# 2) 拉取并合并 upstream（校验 remote tip，报告 ahead/behind）
# 3) 构建前端 + 后端（必须 -tags embed），二进制版本必须等于源码 VERSION
# 4) 原子替换二进制，通过唯一的 systemd 服务重启并校验
#
# VS Code task: "sub2api: update and restart"
#
# "Already up to date" from git merge only means every upstream commit is
# already reachable from HEAD. Local commits may still be ahead, and the
# running binary may still be stale — this script always rebuilds + restarts
# and prints source/binary/running version so that is not mistaken for "live
# process is current".

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
LOCAL_NATIVE="$ROOT/local-native"
BIN_DIR="$LOCAL_NATIVE/build"
RUNTIME="$LOCAL_NATIVE/runtime"
WORK="$RUNTIME/work"
ENV_FILE="$RUNTIME/sub2api.env"
OUT_BIN="$BIN_DIR/sub2api-source"
SERVICE_NAME="${SUB2API_SYSTEMD_SERVICE:-sub2api-source.service}"
VERSION_FILE="$ROOT/backend/cmd/server/VERSION"
UPSTREAM_REMOTE="${SUB2API_UPSTREAM_REMOTE:-upstream}"
UPSTREAM_BRANCH="${SUB2API_UPSTREAM_BRANCH:-main}"
FETCH_ATTEMPTS="${SUB2API_FETCH_ATTEMPTS:-5}"

log() { printf '[sub2api] %s\n' "$*"; }
die() { printf '[sub2api] ERROR: %s\n' "$*" >&2; exit 1; }

read_source_version() {
  [[ -f "$VERSION_FILE" ]] || die "missing source version file: $VERSION_FILE"
  tr -d '[:space:]' <"$VERSION_FILE"
}

# Binary -version prints one line then exits (see backend/cmd/server/main.go).
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

fetch_upstream() {
  local attempt delay
  for attempt in $(seq 1 "$FETCH_ATTEMPTS"); do
    if git fetch "$UPSTREAM_REMOTE" --prune; then
      return 0
    fi
    delay=$((attempt * 2))
    log "fetch $UPSTREAM_REMOTE failed (attempt ${attempt}/${FETCH_ATTEMPTS}); retry in ${delay}s"
    sleep "$delay"
  done
  return 1
}

# Confirm the remote-tracking ref matches the live remote tip. A successful
# HTTP handshake that left refs stale would otherwise make merge lie.
assert_tracking_matches_remote_tip() {
  local merge_ref="$1"
  local remote_tip tracking_tip
  remote_tip="$(git ls-remote "$UPSTREAM_REMOTE" "refs/heads/${UPSTREAM_BRANCH}" | awk 'NR==1 {print $1}')"
  [[ -n "$remote_tip" ]] || die "git ls-remote $UPSTREAM_REMOTE refs/heads/${UPSTREAM_BRANCH} returned empty"
  tracking_tip="$(git rev-parse --verify "$merge_ref")"
  if [[ "$remote_tip" != "$tracking_tip" ]]; then
    die "after fetch, $merge_ref is $tracking_tip but remote tip is $remote_tip — fetch did not update tracking ref"
  fi
}

[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE (copy from runtime/sub2api.env.example)"
command -v go >/dev/null || die "go not found"
command -v pnpm >/dev/null || die "pnpm not found"
command -v git >/dev/null || die "git not found"
command -v curl >/dev/null || die "curl not found"
command -v ss >/dev/null || die "ss not found"
command -v grep >/dev/null || die "grep not found"
command -v systemctl >/dev/null || die "systemctl not found"
command -v sudo >/dev/null || die "sudo not found"

cd "$ROOT"
git rev-parse --is-inside-work-tree >/dev/null || die "not a git repo: $ROOT"

mkdir -p "$BIN_DIR" "$WORK"

SOURCE_VERSION="$(read_source_version)"
log "root=$ROOT source_version=${SOURCE_VERSION}"

# ── 1) Git: stage whole-repo trackable changes + commit ──────
log "git status (pre-commit)"
git status --short || true

# Entire project; .gitignore excludes secrets/build/logs.
# Never force-add ignored paths.
git add -A

if ! git diff --cached --quiet; then
  # Hard refuse if secrets/dumps slipped past ignore rules
  if git diff --cached --name-only | grep -E '(^|/)sub2api\.env$|(^|/)config\.yaml$|\.dump$|\.env$' >/dev/null; then
    die "refusing to commit secrets/dumps; unstage and fix gitignore"
  fi
  BRANCH="$(git rev-parse --abbrev-ref HEAD)"
  MSG="chore: snapshot worktree before update-and-restart (${BRANCH})"
  log "committing whole-repo trackable changes: $MSG"
  git commit -m "$MSG"
else
  log "nothing to commit (clean trackable worktree)"
fi

# ── 2) Fetch + merge upstream ────────────────────────────────
# Stay on the current branch (expected: main tracking upstream/main).
# Same commit-then-merge model as scripts/pull_all_upstreams.sh — do not
# create/switch local/* branches here.
if ! git remote get-url "$UPSTREAM_REMOTE" >/dev/null 2>&1; then
  die "remote '$UPSTREAM_REMOTE' missing — add it (https://github.com/Wei-Shaw/sub2api.git) before update"
fi

log "fetch $UPSTREAM_REMOTE"
fetch_upstream || die "fetch $UPSTREAM_REMOTE failed after ${FETCH_ATTEMPTS} attempts"

CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$CURRENT_BRANCH" == "HEAD" ]]; then
  die "detached HEAD — checkout main (tracking $UPSTREAM_REMOTE/$UPSTREAM_BRANCH) first"
fi

# Prefer configured upstream tracking when it points at the same remote;
# otherwise merge the explicit upstream branch (default: upstream/main).
TRACK_REF="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
if [[ -n "$TRACK_REF" && "$TRACK_REF" == "$UPSTREAM_REMOTE"/* ]]; then
  MERGE_REF="$TRACK_REF"
else
  MERGE_REF="$UPSTREAM_REMOTE/$UPSTREAM_BRANCH"
fi

git rev-parse --verify "$MERGE_REF" >/dev/null || die "missing ref $MERGE_REF after fetch"
assert_tracking_matches_remote_tip "$MERGE_REF"

HEAD_SHA="$(git rev-parse HEAD)"
UPSTREAM_SHA="$(git rev-parse "$MERGE_REF")"
AHEAD="$(git rev-list --count "${MERGE_REF}..HEAD")"
BEHIND="$(git rev-list --count "HEAD..${MERGE_REF}")"
log "git HEAD=${HEAD_SHA:0:12} (${CURRENT_BRANCH})  ${MERGE_REF}=${UPSTREAM_SHA:0:12}  ahead=${AHEAD} behind=${BEHIND}"

if [[ "$BEHIND" -eq 0 ]]; then
  # git merge would print "Already up to date." — that only means upstream
  # commits are already in HEAD. Local may still be ahead; binary may be stale.
  log "no new upstream commits to merge (upstream tip already ancestor of HEAD)"
  if [[ "$AHEAD" -gt 0 ]]; then
    log "local branch is ahead of ${MERGE_REF} by ${AHEAD} commit(s) (local-native / local work)"
  fi
else
  log "merge ${BEHIND} new commit(s) from $MERGE_REF into $CURRENT_BRANCH"
  if ! git merge --no-edit "$MERGE_REF"; then
    die "merge conflict with $MERGE_REF — resolve manually, then: git merge --continue && re-run"
  fi
fi

SOURCE_VERSION="$(read_source_version)"
log "source_version after merge: ${SOURCE_VERSION}  HEAD=$(git rev-parse --short HEAD)"

# ── 3) Build frontend ────────────────────────────────────────
log "building frontend…"
if [[ ! -d "$ROOT/frontend/node_modules" ]]; then
  pnpm --dir "$ROOT/frontend" install
fi

WEB_DIST="$ROOT/backend/internal/web/dist"
# Vite outDir is backend/internal/web/dist (see frontend/vite.config).
# Do NOT delete WEB_DIST after build — that wiped the embed payload previously.
pnpm --dir "$ROOT/frontend" run build

if [[ ! -f "$WEB_DIST/index.html" ]]; then
  die "frontend dist missing index.html at $WEB_DIST (check frontend/vite.config outDir)"
fi

# ── 4) Build backend with -tags embed ────────────────────────
log "building backend (-tags embed)…"
VERSION="$(cd "$ROOT/backend" && ./scripts/resolve-version.sh)"
[[ -n "$VERSION" ]] || die "resolve-version.sh returned empty"
if [[ "$VERSION" != "$SOURCE_VERSION" ]]; then
  die "resolve-version.sh => ${VERSION} but ${VERSION_FILE} => ${SOURCE_VERSION}"
fi
LDFLAGS="-s -w -X main.Version=${VERSION}"
TMP_BIN="$BIN_DIR/sub2api-source.new"
(
  cd "$ROOT/backend"
  CGO_ENABLED=0 go build -tags embed -ldflags="$LDFLAGS" -trimpath -o "$TMP_BIN" ./cmd/server
)

if strings "$TMP_BIN" | grep -F 'Frontend not embedded' >/dev/null; then
  rm -f "$TMP_BIN"
  die "binary lacks embed frontend (build without -tags embed)"
fi

BIN_VERSION="$(binary_version "$TMP_BIN")" || die "cannot read version from new binary via -version"
if [[ "$BIN_VERSION" != "$SOURCE_VERSION" ]]; then
  rm -f "$TMP_BIN"
  die "new binary version ${BIN_VERSION} != source ${SOURCE_VERSION}"
fi
log "built binary version=${BIN_VERSION}"

# ── 5) Deploy and restart the single service owner ──────────
# Load env first so SERVER_PORT matches the running instance.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a
PORT="${SERVER_PORT:-18081}"

systemctl cat "$SERVICE_NAME" >/dev/null 2>&1 ||
  die "missing canonical system service: $SERVICE_NAME"
systemctl is-enabled --quiet "$SERVICE_NAME" ||
  die "canonical system service is not enabled: $SERVICE_NAME"

# Refuse split ownership instead of killing arbitrary listeners. The only
# accepted existing listener is the current MainPID of the canonical service.
CURRENT_MAIN_PID="$(systemctl show "$SERVICE_NAME" -p MainPID --value)"
mapfile -t CURRENT_LISTEN_PIDS < <(
  ss -ltnp "sport = :${PORT}" 2>/dev/null |
    grep -oE 'pid=[0-9]+' |
    cut -d= -f2 |
    sort -u || true
)
for p in "${CURRENT_LISTEN_PIDS[@]:-}"; do
  [[ -z "${p:-}" ]] && continue
  if [[ "$CURRENT_MAIN_PID" == "0" || "$p" != "$CURRENT_MAIN_PID" ]]; then
    die "port ${PORT} is owned by pid ${p}, not ${SERVICE_NAME} MainPID ${CURRENT_MAIN_PID}; repair duplicate service ownership first"
  fi
done

if [[ -f "$OUT_BIN" ]]; then
  cp -af "$OUT_BIN" "$BIN_DIR/sub2api-source.previous"
fi
mv -f "$TMP_BIN" "$OUT_BIN"
chmod +x "$OUT_BIN"

log "restarting canonical service ${SERVICE_NAME} with ${OUT_BIN} (version ${BIN_VERSION})…"
sudo systemctl restart "$SERVICE_NAME"

for _ in $(seq 1 40); do
  if curl -fsS -m 1 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    break
  fi
  if ! systemctl is-active --quiet "$SERVICE_NAME"; then
    sudo journalctl -u "$SERVICE_NAME" -n 50 --no-pager >&2 || true
    die "$SERVICE_NAME exited during startup"
  fi
  sleep 0.5
done

ROOT_CODE=$(curl -sS -m 5 -o /tmp/sub2api-root.html -w "%{http_code}" "http://127.0.0.1:${PORT}/" || echo 000)
HEALTH_CODE=$(curl -sS -m 5 -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/health" || echo 000)
NEW_PID="$(systemctl show "$SERVICE_NAME" -p MainPID --value)"
[[ "$NEW_PID" =~ ^[1-9][0-9]*$ ]] || die "invalid MainPID for $SERVICE_NAME: $NEW_PID"

# Confirm the listener is owned exclusively by the canonical service.
mapfile -t LISTEN_PIDS < <(ss -ltnp "sport = :${PORT}" 2>/dev/null | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u || true)
if [[ ${#LISTEN_PIDS[@]} -ne 1 || "${LISTEN_PIDS[0]}" != "$NEW_PID" ]]; then
  die "port ${PORT} listener pids=[${LISTEN_PIDS[*]:-}] must equal $SERVICE_NAME MainPID ${NEW_PID}"
fi

log "health=${HEALTH_CODE} root=${ROOT_CODE} pid=${NEW_PID} version=${BIN_VERSION} source=${SOURCE_VERSION}"

[[ "$HEALTH_CODE" == "200" ]] || die "health check failed"
if [[ "$ROOT_CODE" != "200" ]] || ! grep -Ei '<!doctype html>|<title>' /tmp/sub2api-root.html >/dev/null; then
  head -c 200 /tmp/sub2api-root.html >&2 || true
  echo >&2
  die "frontend not serving HTML (HTTP $ROOT_CODE) — embed build broken?"
fi

log "OK — upstream synced (ahead=${AHEAD} behind=0), built ${BIN_VERSION} with embed, restarted ${SERVICE_NAME} pid=${NEW_PID} on :${PORT}"
