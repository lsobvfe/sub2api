#!/usr/bin/env bash
# 1) git 提交**整个仓库**当前可跟踪变更（gitignore 排除密钥/构建物）
# 2) 拉取 upstream 更新
# 3) 构建前端 + 后端（必须 -tags embed）
# 4) 重启 local-native 进程
#
# VS Code task: "sub2api: update and restart"

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOCAL_NATIVE="$ROOT/local-native"
BIN_DIR="$LOCAL_NATIVE/build"
RUNTIME="$LOCAL_NATIVE/runtime"
WORK="$RUNTIME/work"
ENV_FILE="$RUNTIME/sub2api.env"
LOG="$LOCAL_NATIVE/logs/sub2api-native.log"
PID_FILE="$RUNTIME/sub2api-native.pid"
OUT_BIN="$BIN_DIR/sub2api-source"
UPSTREAM_REMOTE="${SUB2API_UPSTREAM_REMOTE:-upstream}"
UPSTREAM_BRANCH="${SUB2API_UPSTREAM_BRANCH:-main}"

log() { printf '[sub2api] %s\n' "$*"; }
die() { printf '[sub2api] ERROR: %s\n' "$*" >&2; exit 1; }

[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE (copy from runtime/sub2api.env.example)"
command -v go >/dev/null || die "go not found"
command -v pnpm >/dev/null || die "pnpm not found"
command -v git >/dev/null || die "git not found"
command -v curl >/dev/null || die "curl not found"

cd "$ROOT"
git rev-parse --is-inside-work-tree >/dev/null || die "not a git repo: $ROOT"

mkdir -p "$BIN_DIR" "$WORK" "$(dirname "$LOG")"

# ── 1) Git: stage whole-repo trackable changes + commit ──────
log "git status (pre-commit)"
git status --short || true

# Entire project; .gitignore excludes secrets/build/logs.
# Never force-add ignored paths.
git add -A

if ! git diff --cached --quiet; then
  # Hard refuse if secrets/dumps slipped past ignore rules
  if git diff --cached --name-only | rg -q '(^|/)sub2api\.env$|(^|/)config\.yaml$|\.dump$|\.env$'; then
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
if git remote get-url "$UPSTREAM_REMOTE" >/dev/null 2>&1; then
  log "fetch $UPSTREAM_REMOTE"
  git fetch "$UPSTREAM_REMOTE" --prune
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
  log "merge $MERGE_REF into $CURRENT_BRANCH"
  if ! git merge --no-edit "$MERGE_REF"; then
    die "merge conflict with $MERGE_REF — resolve manually, then: git merge --continue && re-run"
  fi
else
  log "WARNING: remote '$UPSTREAM_REMOTE' missing; skip pull"
fi

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
  # Fallback if outDir ever points at frontend/dist
  if [[ -f "$ROOT/frontend/dist/index.html" ]]; then
    rm -rf "$WEB_DIST"
    mkdir -p "$WEB_DIST"
    cp -a "$ROOT/frontend/dist/." "$WEB_DIST/"
  fi
fi
[[ -f "$WEB_DIST/index.html" ]] || die "frontend dist missing index.html at $WEB_DIST"

# ── 4) Build backend with -tags embed ────────────────────────
log "building backend (-tags embed)…"
VERSION="$(cd "$ROOT/backend" && ./scripts/resolve-version.sh 2>/dev/null || date +%Y%m%d%H%M)"
LDFLAGS="-s -w -X main.Version=${VERSION}"
TMP_BIN="$BIN_DIR/sub2api-source.new"
(
  cd "$ROOT/backend"
  CGO_ENABLED=0 go build -tags embed -ldflags="$LDFLAGS" -trimpath -o "$TMP_BIN" ./cmd/server
)

if strings "$TMP_BIN" | rg -q 'Frontend not embedded'; then
  rm -f "$TMP_BIN"
  die "binary lacks embed frontend (build without -tags embed)"
fi

# ── 5) Restart ───────────────────────────────────────────────
log "stopping old process…"
mapfile -t OLD_PIDS < <(pgrep -x sub2api-source || true)
for p in "${OLD_PIDS[@]:-}"; do
  [[ -z "${p:-}" ]] && continue
  kill "$p" 2>/dev/null || true
done
sleep 2
for p in "${OLD_PIDS[@]:-}"; do
  [[ -z "${p:-}" ]] && continue
  if [[ -d "/proc/$p" ]]; then
    kill -9 "$p" 2>/dev/null || true
  fi
done
sleep 1

if [[ -f "$OUT_BIN" ]]; then
  cp -af "$OUT_BIN" "$BIN_DIR/sub2api-source.previous"
fi
mv -f "$TMP_BIN" "$OUT_BIN"
chmod +x "$OUT_BIN"

log "starting…"
cd "$WORK"
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a
nohup "$OUT_BIN" >>"$LOG" 2>&1 &
echo $! | tee "$PID_FILE"
NEW_PID=$(cat "$PID_FILE")
PORT="${SERVER_PORT:-18081}"

for _ in $(seq 1 40); do
  if curl -fsS -m 1 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$NEW_PID" 2>/dev/null; then
    tail -50 "$LOG" >&2 || true
    die "process exited during startup"
  fi
  sleep 0.5
done

ROOT_CODE=$(curl -sS -m 5 -o /tmp/sub2api-root.html -w "%{http_code}" "http://127.0.0.1:${PORT}/" || echo 000)
HEALTH_CODE=$(curl -sS -m 5 -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/health" || echo 000)
log "health=$HEALTH_CODE root=$ROOT_CODE pid=$NEW_PID"

[[ "$HEALTH_CODE" == "200" ]] || die "health check failed"
if [[ "$ROOT_CODE" != "200" ]] || ! rg -q '<!doctype html>|<title>' /tmp/sub2api-root.html; then
  head -c 200 /tmp/sub2api-root.html >&2 || true
  echo >&2
  die "frontend not serving HTML (HTTP $ROOT_CODE) — embed build broken?"
fi

log "OK — committed (if needed), upstream merged, built with embed, restarted"
