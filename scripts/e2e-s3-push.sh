#!/usr/bin/env bash
#
# e2e-s3-push.sh — full end-to-end test of bucketvcs against a real Amazon S3 bucket.
#
# Runs the documented Quickstart flow (docs/quickstart-s3.md + docs/quickstart.md
# §4-6) as one script: create a fresh auth DB, create a new repo in S3, start the
# gateway, clone an existing repo from GitHub, push it to bucketvcs (stored in
# S3), then clone it back and verify the history round-tripped.
#
# Reads S3_STORE (the s3:// URL) and AWS credentials from your .envrc (sourced if
# not already exported). The bucket region is resolved from
# BUCKETVCS_S3_REGION/AWS_REGION, else auto-detected via the AWS CLI, else
# defaults to us-east-1. A throwaway auth DB is created per run.
#
# Usage:
#   scripts/e2e-s3-push.sh [SRC_REPO] [--keep]
#
#   SRC_REPO   GitHub (or any) repo URL to clone as the push source.
#              Default: https://github.com/octocat/Hello-World.git
#   --keep     Do NOT purge the test repo from S3 on exit (default: purge).
#
# Overridable via env: TENANT, REPO, ADDR, USER_NAME, BVCS (binary path),
#                      AWS_REGION/BUCKETVCS_S3_REGION, SRC_REPO (same as positional).
#
set -euo pipefail

# --- locate repo root (this script lives in scripts/) -----------------------
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- tiny logging helpers ---------------------------------------------------
if [ -t 1 ]; then BLU=$'\033[34m'; GRN=$'\033[32m'; RED=$'\033[31m'; DIM=$'\033[2m'; RST=$'\033[0m'
else BLU=; GRN=; RED=; DIM=; RST=; fi
step() { printf '\n%s==>%s %s\n' "$BLU" "$RST" "$*"; }
info() { printf '%s    %s%s\n' "$DIM" "$*" "$RST"; }
ok()   { printf '%s  ✓ %s%s\n' "$GRN" "$*" "$RST"; }
die()  { printf '%s  ✗ %s%s\n' "$RED" "$*" "$RST" >&2; exit 1; }

# Return a TCP port on 127.0.0.1 that nothing is currently listening on.
pick_free_port() {
  local p
  for p in $(seq 8090 8200); do
    ss -ltn 2>/dev/null | grep -q ":$p " && continue
    (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null && { exec 3>&- 3<&-; continue; }
    echo "$p"; return 0
  done
  return 1
}

# --- argument parsing -------------------------------------------------------
KEEP=0
SRC_REPO="${SRC_REPO:-}"
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    -h|--help) sed -n '3,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    -*) die "unknown flag: $arg" ;;
    *)  SRC_REPO="$arg" ;;
  esac
done
SRC_REPO="${SRC_REPO:-https://github.com/octocat/Hello-World.git}"

# --- load env from .envrc if S3_STORE not already in the environment --------
if [ -z "${S3_STORE:-}" ] && [ -f "$ROOT/.envrc" ]; then
  step "Loading env from .envrc"
  # .envrc is a plain set of `export` lines; sourcing it is safe.
  # shellcheck disable=SC1091
  set -a; . "$ROOT/.envrc"; set +a
  info "sourced $ROOT/.envrc"
fi

# --- validate required configuration ----------------------------------------
step "Validating configuration"
# This script is S3-specific: prefer S3_STORE, ignore any gcs:// STORE from .envrc.
STORE="${S3_STORE:-${STORE:-}}"
[ -n "$STORE" ] || die "S3_STORE is not set (expected an s3:// URL, e.g. s3://my-bucket). Set it or add it to .envrc."
case "$STORE" in
  s3://*) : ;;
  *) die "this is the S3 e2e test but the store '$STORE' is not an s3:// URL." ;;
esac

# AWS credentials: static key (env), shared profile, or ambient role chain.
if [ -n "${AWS_ACCESS_KEY_ID:-}" ]; then
  [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || die "AWS_ACCESS_KEY_ID is set but AWS_SECRET_ACCESS_KEY is missing"
  info "AWS static credentials: ${AWS_ACCESS_KEY_ID:0:4}…"
elif [ -n "${AWS_PROFILE:-${BUCKETVCS_S3_PROFILE:-}}" ]; then
  info "AWS profile: ${AWS_PROFILE:-$BUCKETVCS_S3_PROFILE}"
else
  info "no static key/profile set — relying on the ambient AWS credential chain (role/IRSA)"
fi

# Region is REQUIRED for s3://. Resolve env → AWS-CLI bucket-location → default.
BUCKET="${STORE#s3://}"; BUCKET="${BUCKET%%/*}"
REGION="${BUCKETVCS_S3_REGION:-${AWS_REGION:-}}"
if [ -z "$REGION" ]; then
  if command -v aws >/dev/null 2>&1; then
    loc="$(aws s3api get-bucket-location --bucket "$BUCKET" --query 'LocationConstraint' --output text 2>/dev/null || true)"
    case "$loc" in
      ""|None|null) REGION="us-east-1" ;;   # us-east-1 reports an empty/None constraint
      *)            REGION="$loc" ;;
    esac
    info "auto-detected region for $BUCKET: $REGION"
  else
    REGION="us-east-1"
    info "no region set and aws CLI absent; defaulting to $REGION (override with AWS_REGION)"
  fi
fi
# bucketvcs reads the region from this env var (see cmd/bucketvcs/store.go).
export BUCKETVCS_S3_REGION="$REGION"
command -v git >/dev/null || die "git not found on PATH"
info "STORE=$STORE  REGION=$REGION"

# --- config (overridable via env) -------------------------------------------
TENANT="${TENANT:-e2e}"
REPO="${REPO:-demo-$(date +%Y%m%d-%H%M%S)-$$}"   # unique per run → no S3 collisions
# Default to a verified-free loopback port (8080/8081 etc. may be taken by other apps).
if [ -n "${ADDR:-}" ]; then
  HOST="${ADDR%:*}"; PORT="${ADDR##*:}"
else
  HOST="127.0.0.1"; PORT="$(pick_free_port)" || die "no free TCP port in 8090-8200"
  ADDR="$HOST:$PORT"
fi
USER_NAME="${USER_NAME:-alice}"
SLUG="$TENANT/$REPO"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/bvcs-e2e.XXXXXX")"
DB="$WORK/auth.db"
SERVE_LOG="$WORK/serve.log"
SERVE_PID=""
REPO_REGISTERED=0
PASSED=0

# --- teardown (always runs) -------------------------------------------------
cleanup() {
  local rc=$?
  step "Teardown"
  if [ -n "$SERVE_PID" ] && kill -0 "$SERVE_PID" 2>/dev/null; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
    info "stopped gateway (pid $SERVE_PID)"
  fi
  if [ "$REPO_REGISTERED" = 1 ]; then
    if [ "$KEEP" = 1 ]; then
      info "--keep: leaving $SLUG in $STORE (delete later with:"
      info "  $BVCS repo delete $SLUG --auth-db <db> --purge-storage --store \"$STORE\" )"
    else
      info "purging $SLUG from S3"
      "$BVCS" repo delete "$SLUG" --auth-db "$DB" --purge-storage --store "$STORE" \
        >/dev/null 2>&1 && info "purged $SLUG" || info "purge failed (clean up manually): $SLUG"
    fi
  fi
  if [ "$PASSED" != 1 ] && [ -f "$SERVE_LOG" ]; then
    printf '%s--- last 20 lines of serve log ---%s\n' "$DIM" "$RST" >&2
    tail -n 20 "$SERVE_LOG" >&2 || true
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

# --- build ------------------------------------------------------------------
BVCS="${BVCS:-$ROOT/bin/bucketvcs}"
if [ ! -x "$BVCS" ] || [ -z "${BVCS_SKIP_BUILD:-}" ]; then
  step "Building bucketvcs (make build)"
  make -C "$ROOT" build >/dev/null
  BVCS="$ROOT/bin/bucketvcs"
fi
[ -x "$BVCS" ] || die "bucketvcs binary not found/executable at $BVCS"
ok "binary ready: $BVCS"

# git over HTTP, non-interactive, no on-disk credential caching (token stays off disk).
git_ni() { GIT_TERMINAL_PROMPT=0 git -c credential.helper= "$@"; }

# === 1. Clone an existing repo from GitHub (the source of truth) ============
step "Cloning source from GitHub: $SRC_REPO"
SRC="$WORK/src"
git_ni clone --quiet "$SRC_REPO" "$SRC"
BRANCH="$(git -C "$SRC" symbolic-ref --short HEAD)"
SRC_HEAD="$(git -C "$SRC" rev-parse HEAD)"
ok "cloned $SRC_REPO (default branch '$BRANCH', HEAD ${SRC_HEAD:0:12})"

# === 2. Create the repo in S3 with a MATCHING default branch ================
# The repo's default branch must be one we actually push: the lazy-mirror
# exporter (used to serve clone/fetch) hard-fails if the configured default
# branch has no commits. So pin it to the source's default branch ('$BRANCH')
# instead of init's built-in 'refs/heads/main'. init creates the storage;
# `repo register --no-init` then adds the auth-registry row.
step "Creating repo $SLUG in $STORE (default branch refs/heads/$BRANCH)"
"$BVCS" init --store "$STORE" --default-branch "refs/heads/$BRANCH" "$TENANT" "$REPO"
"$BVCS" repo register "$SLUG" --auth-db "$DB" --no-init
REPO_REGISTERED=1
ok "created + registered $SLUG"

step "Sanity check: inspect-manifest"
"$BVCS" inspect-manifest --store "$STORE" "$TENANT" "$REPO" >/dev/null
ok "manifest readable in S3"

# === 3. Access: user, token, grant =========================================
step "Creating user, token, grant"
"$BVCS" user add "$USER_NAME" --auth-db "$DB" >/dev/null
TOKEN="$("$BVCS" token create "$USER_NAME" --auth-db "$DB" \
          --scopes=repo:read,repo:write --label=e2e | sed -n 's/^token=//p')"
[ -n "$TOKEN" ] || die "failed to capture token from 'token create' output"
"$BVCS" repo grant "$USER_NAME" "$SLUG" write --auth-db "$DB"
ok "user=$USER_NAME, token captured (repo:read,write), write granted"

# === 4. Start the gateway ===================================================
step "Starting gateway on $ADDR"
# --lfs=false: this test exercises Git push/clone, not LFS (and --lfs=true would
# require the proxied-URL signing config). --mirror-dir keeps the run self-contained.
"$BVCS" serve --store "$STORE" --auth-db "$DB" --addr "$ADDR" --lfs=false \
  --mirror-dir "$WORK/mirror" >"$SERVE_LOG" 2>&1 &
SERVE_PID=$!

# Authenticated remote URL. The gateway routes on a required ".git" suffix
# (/<tenant>/<repo>.git).
REMOTE="http://$USER_NAME:$TOKEN@$ADDR/$SLUG.git"

# Readiness: poll ls-remote against our own repo. This confirms the listener is
# actually bucketvcs serving $SLUG (not some other process on the port) and that
# auth works — a far stronger signal than a raw TCP connect.
ready=0
for _ in $(seq 1 100); do
  kill -0 "$SERVE_PID" 2>/dev/null || die "gateway exited early; see log above"
  if git_ni ls-remote "$REMOTE" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" = 1 ] || die "gateway did not become ready on $ADDR"
ok "gateway listening + serving $SLUG (pid $SERVE_PID)"

# === 5. Push to bucketvcs (→ S3) ============================================
step "Pushing to bucketvcs → S3"
git -C "$SRC" remote add bucketvcs "$REMOTE"
git_ni -C "$SRC" push --quiet -u bucketvcs "$BRANCH"
git_ni -C "$SRC" push --quiet bucketvcs --tags
ok "pushed branch '$BRANCH' and tags"

# === 6. Make a local change, commit it, and push it (incremental push) ======
step "Committing a local change and pushing it"
MARKER="e2e-marker-$(date +%Y%m%d-%H%M%S)-$$.txt"
printf 'bucketvcs e2e incremental commit\nrepo: %s\nwhen: %s\n' \
  "$SLUG" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$SRC/$MARKER"
git -C "$SRC" add "$MARKER"
git -C "$SRC" -c user.email='e2e@bucketvcs.test' -c user.name='bucketvcs e2e' \
  commit --quiet -m "e2e: add $MARKER"
NEW_HEAD="$(git -C "$SRC" rev-parse HEAD)"
git_ni -C "$SRC" push --quiet bucketvcs "$BRANCH"
ok "committed $MARKER and pushed (new HEAD ${NEW_HEAD:0:12})"

# === 7. Verify: server tip + round-trip clone of the new commit =============
step "Verifying refs on the server"
LS="$(git_ni ls-remote "$REMOTE")"
TIP="$(printf '%s\n' "$LS" | awk -v b="refs/heads/$BRANCH" '$2==b {print $1}')"
[ "$TIP" = "$NEW_HEAD" ] || die "server tip for $BRANCH is '$TIP', expected new commit $NEW_HEAD"
info "$(printf '%s\n' "$LS" | wc -l | tr -d ' ') refs advertised; $BRANCH tip = ${TIP:0:12}"
ok "branch refs/heads/$BRANCH advertises the new commit"

step "Round-trip: cloning back from bucketvcs"
RT="$WORK/roundtrip"
git_ni clone --quiet "$REMOTE" "$RT"
RT_HEAD="$(git -C "$RT" rev-parse HEAD)"
[ "$RT_HEAD" = "$NEW_HEAD" ] || die "round-trip HEAD mismatch: pushed=$NEW_HEAD got=$RT_HEAD"
[ -f "$RT/$MARKER" ] || die "round-trip clone is missing the pushed file $MARKER"
ok "round-trip HEAD matches the new commit and contains $MARKER"

# Commit-graph parity: clone-back equals the post-change source.
SRC_LOG="$(git -C "$SRC" rev-list --count "$BRANCH")"
RT_LOG="$(git -C "$RT" rev-list --count HEAD)"
[ "$SRC_LOG" = "$RT_LOG" ] || die "commit count mismatch: source=$SRC_LOG bucketvcs=$RT_LOG"
ok "commit count matches ($SRC_LOG commits, incl. the new one)"

PASSED=1
step "PASS"
printf '%s  bucketvcs e2e (S3) succeeded: %s pushed through %s and verified.%s\n' \
  "$GRN" "$SRC_REPO" "$STORE" "$RST"
