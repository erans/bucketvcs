# shellcheck shell=bash
#
# scripts/lib/e2e-common.sh — shared flow for the cloud e2e push/clone tests.
#
# Sourced by scripts/e2e-s3-push.sh and scripts/e2e-gcs-push.sh. Each wrapper
# does backend-specific setup (load .envrc, validate + export the store/creds,
# set STORE and BACKEND), then calls `e2e_run`.
#
# Contract expected by e2e_run (set by the wrapper before calling it):
#   ROOT     repo root (absolute)
#   STORE    validated store URL (e.g. s3://bucket or gcs://bucket)
#   BACKEND  short label for messages (e.g. "S3", "GCS")
#   KEEP / SRC_REPO   set by e2e_parse_args
#   plus any backend env the bucketvcs binary needs (AWS_*, BUCKETVCS_GCS_*, …)
#
# Overridable via env: TENANT, REPO, ADDR, USER_NAME, BVCS, BVCS_SKIP_BUILD.

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

# git over HTTP, non-interactive, no on-disk credential caching (token off disk).
git_ni() { GIT_TERMINAL_PROMPT=0 git -c credential.helper= "$@"; }

# e2e_parse_args "$@" — sets globals KEEP and SRC_REPO.
e2e_parse_args() {
  KEEP=0
  SRC_REPO="${SRC_REPO:-}"
  local arg
  for arg in "$@"; do
    case "$arg" in
      --keep) KEEP=1 ;;
      -h|--help)
        printf 'Usage: %s [SRC_REPO] [--keep]\n\n' "$(basename "${0:-e2e}")"
        printf '  SRC_REPO   GitHub (or any) repo URL to clone as the push source.\n'
        printf '             Default: https://github.com/octocat/Hello-World.git\n'
        printf '  --keep     Do NOT purge the test repo from object storage on exit.\n'
        exit 0 ;;
      -*) die "unknown flag: $arg" ;;
      *)  SRC_REPO="$arg" ;;
    esac
  done
  SRC_REPO="${SRC_REPO:-https://github.com/octocat/Hello-World.git}"
}

# e2e_cleanup — EXIT trap installed by e2e_run. Uses globals it sets.
e2e_cleanup() {
  local rc=$?
  step "Teardown"
  if [ -n "${SERVE_PID:-}" ] && kill -0 "$SERVE_PID" 2>/dev/null; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
    info "stopped gateway (pid $SERVE_PID)"
  fi
  if [ "${REPO_REGISTERED:-0}" = 1 ]; then
    if [ "${KEEP:-0}" = 1 ]; then
      info "--keep: leaving $SLUG in $STORE (delete later with:"
      info "  $BVCS repo delete $SLUG --auth-db <db> --purge-storage --store \"$STORE\" )"
    else
      info "purging $SLUG from $BACKEND"
      "$BVCS" repo delete "$SLUG" --auth-db "$DB" --purge-storage --store "$STORE" \
        >/dev/null 2>&1 && info "purged $SLUG" || info "purge failed (clean up manually): $SLUG"
    fi
  fi
  if [ "${PASSED:-0}" != 1 ] && [ -n "${SERVE_LOG:-}" ] && [ -f "$SERVE_LOG" ]; then
    printf '%s--- last 20 lines of serve log ---%s\n' "$DIM" "$RST" >&2
    tail -n 20 "$SERVE_LOG" >&2 || true
  fi
  [ -n "${WORK:-}" ] && rm -rf "$WORK"
  exit "$rc"
}

# e2e_run — the full backend-neutral flow. Requires STORE + BACKEND + ROOT set.
e2e_run() {
  command -v git >/dev/null || die "git not found on PATH"
  : "${STORE:?e2e_run: STORE not set by wrapper}"
  : "${BACKEND:?e2e_run: BACKEND not set by wrapper}"
  : "${ROOT:?e2e_run: ROOT not set by wrapper}"

  # --- config (overridable via env) -----------------------------------------
  TENANT="${TENANT:-e2e}"
  REPO="${REPO:-demo-$(date +%Y%m%d-%H%M%S)-$$}"   # unique per run → no collisions
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
  trap e2e_cleanup EXIT

  # --- build ----------------------------------------------------------------
  BVCS="${BVCS:-$ROOT/bin/bucketvcs}"
  if [ ! -x "$BVCS" ] || [ -z "${BVCS_SKIP_BUILD:-}" ]; then
    step "Building bucketvcs (make build)"
    make -C "$ROOT" build >/dev/null
    BVCS="$ROOT/bin/bucketvcs"
  fi
  [ -x "$BVCS" ] || die "bucketvcs binary not found/executable at $BVCS"
  ok "binary ready: $BVCS"

  # === 1. Clone an existing repo from GitHub (the source of truth) ==========
  step "Cloning source from GitHub: $SRC_REPO"
  SRC="$WORK/src"
  git_ni clone --quiet "$SRC_REPO" "$SRC"
  BRANCH="$(git -C "$SRC" symbolic-ref --short HEAD)"
  SRC_HEAD="$(git -C "$SRC" rev-parse HEAD)"
  ok "cloned $SRC_REPO (default branch '$BRANCH', HEAD ${SRC_HEAD:0:12})"

  # === 2. Create the repo with a MATCHING default branch ====================
  # The repo's default branch must be one we actually push: the lazy-mirror
  # exporter (used to serve clone/fetch) hard-fails if the configured default
  # branch has no commits. So pin it to the source's default branch instead of
  # init's built-in 'refs/heads/main'. init creates the storage; `repo register
  # --no-init` then adds the auth-registry row.
  step "Creating repo $SLUG in $STORE (default branch refs/heads/$BRANCH)"
  "$BVCS" init --store "$STORE" --default-branch "refs/heads/$BRANCH" "$TENANT" "$REPO"
  "$BVCS" repo register "$SLUG" --auth-db "$DB" --no-init
  REPO_REGISTERED=1
  ok "created + registered $SLUG"

  step "Sanity check: inspect-manifest"
  "$BVCS" inspect-manifest --store "$STORE" "$TENANT" "$REPO" >/dev/null
  ok "manifest readable in $BACKEND"

  # === 3. Access: user, token, grant ========================================
  step "Creating user, token, grant"
  "$BVCS" user add "$USER_NAME" --auth-db "$DB" >/dev/null
  TOKEN="$("$BVCS" token create "$USER_NAME" --auth-db "$DB" \
            --scopes=repo:read,repo:write,lfs:read,lfs:write --label=e2e | sed -n 's/^token=//p')"
  [ -n "$TOKEN" ] || die "failed to capture token from 'token create' output"
  "$BVCS" repo grant "$USER_NAME" "$SLUG" write --auth-db "$DB"
  ok "user=$USER_NAME, token captured (repo + lfs read/write), write granted"

  # === 4. Start the gateway (LFS enabled) ===================================
  # LFS needs --lfs plus a proxied-URL signing key + base URL. For real S3/GCS
  # the transfers go DIRECT (bucketvcs mints a presigned PUT/GET straight to the
  # bucket); the proxied path is wired but should not fire. --mirror-dir keeps
  # the run self-contained.
  step "Starting gateway on $ADDR"
  SIGNING_KEY="$WORK/signing.key"
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$SIGNING_KEY"   # 64 hex chars (>=16 bytes)
  "$BVCS" serve --store "$STORE" --auth-db "$DB" --addr "$ADDR" \
    --lfs --proxied-url-signing-key "$SIGNING_KEY" --proxied-url-base "http://$ADDR" \
    --mirror-dir "$WORK/mirror" >"$SERVE_LOG" 2>&1 &
  SERVE_PID=$!

  # Authenticated remote URL. The gateway routes on a required ".git" suffix.
  REMOTE="http://$USER_NAME:$TOKEN@$ADDR/$SLUG.git"

  # Readiness: poll ls-remote against our own repo — confirms the listener is
  # actually bucketvcs serving $SLUG (not some other process on the port) and
  # that auth works, a far stronger signal than a raw TCP connect.
  ready=0
  for _ in $(seq 1 100); do
    kill -0 "$SERVE_PID" 2>/dev/null || die "gateway exited early; see log above"
    if git_ni ls-remote "$REMOTE" >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.1
  done
  [ "$ready" = 1 ] || die "gateway did not become ready on $ADDR"
  ok "gateway listening + serving $SLUG (pid $SERVE_PID)"

  # === 5. Push to bucketvcs =================================================
  step "Pushing to bucketvcs → $BACKEND"
  git -C "$SRC" remote add bucketvcs "$REMOTE"
  git_ni -C "$SRC" push --quiet -u bucketvcs "$BRANCH"
  git_ni -C "$SRC" push --quiet bucketvcs --tags
  ok "pushed branch '$BRANCH' and tags"

  # === 6. Make a local change on the default branch, commit, and push =======
  step "Committing a change on '$BRANCH' and pushing it"
  MARKER1="e2e-main-$(date +%H%M%S)-$$.txt"
  printf 'bucketvcs e2e — change on default branch %s\nrepo: %s\n' "$BRANCH" "$SLUG" > "$SRC/$MARKER1"
  git -C "$SRC" add "$MARKER1"
  git -C "$SRC" -c user.email='e2e@bucketvcs.test' -c user.name='bucketvcs e2e' \
    commit --quiet -m "e2e: change on $BRANCH ($MARKER1)"
  MAIN_HEAD="$(git -C "$SRC" rev-parse HEAD)"
  git_ni -C "$SRC" push --quiet bucketvcs "$BRANCH"
  ok "pushed '$BRANCH' (tip ${MAIN_HEAD:0:12})"

  # === 7. Create a SECOND branch with its own change, commit, and push ======
  BRANCH2="e2e-feature-$(date +%H%M%S)-$$"
  step "Creating branch '$BRANCH2' with its own change and pushing it"
  git -C "$SRC" checkout --quiet -b "$BRANCH2"
  MARKER2="e2e-feature-$(date +%H%M%S)-$$.txt"
  printf 'bucketvcs e2e — change on feature branch %s\nrepo: %s\n' "$BRANCH2" "$SLUG" > "$SRC/$MARKER2"
  git -C "$SRC" add "$MARKER2"
  git -C "$SRC" -c user.email='e2e@bucketvcs.test' -c user.name='bucketvcs e2e' \
    commit --quiet -m "e2e: change on $BRANCH2 ($MARKER2)"
  FEAT_HEAD="$(git -C "$SRC" rev-parse HEAD)"
  git_ni -C "$SRC" push --quiet bucketvcs "$BRANCH2"
  ok "pushed '$BRANCH2' (tip ${FEAT_HEAD:0:12})"
  [ "$MAIN_HEAD" != "$FEAT_HEAD" ] || die "the two branches unexpectedly point at the same commit"

  # === 8. Nuke the local repo, clone fresh, verify BOTH branches differ =====
  step "Deleting the local repo and cloning fresh from bucketvcs"
  rm -rf "$SRC"
  FRESH="$WORK/fresh"
  git_ni clone --quiet "$REMOTE" "$FRESH"

  DEF="$(git -C "$FRESH" symbolic-ref --short HEAD)"
  [ "$DEF" = "$BRANCH" ] || die "fresh clone default branch is '$DEF', expected '$BRANCH'"

  got_main="$(git -C "$FRESH" rev-parse --verify --quiet "origin/$BRANCH"  || true)"
  got_feat="$(git -C "$FRESH" rev-parse --verify --quiet "origin/$BRANCH2" || true)"
  [ -n "$got_main" ] || die "fresh clone is missing branch '$BRANCH'"
  [ -n "$got_feat" ] || die "fresh clone is missing branch '$BRANCH2'"
  [ "$got_main" = "$MAIN_HEAD" ] || die "'$BRANCH' tip is $got_main, expected $MAIN_HEAD"
  [ "$got_feat" = "$FEAT_HEAD" ] || die "'$BRANCH2' tip is $got_feat, expected $FEAT_HEAD"
  [ "$got_main" != "$got_feat" ] || die "the two branches resolve to the same commit"
  ok "fresh clone has both branches with distinct tips:"
  info "  $BRANCH → ${got_main:0:12}"
  info "  $BRANCH2 → ${got_feat:0:12}"

  git -C "$FRESH" cat-file -e "origin/$BRANCH:$MARKER1"  2>/dev/null || die "'$BRANCH' missing $MARKER1"
  git -C "$FRESH" cat-file -e "origin/$BRANCH2:$MARKER2" 2>/dev/null || die "'$BRANCH2' missing $MARKER2"
  ! git -C "$FRESH" cat-file -e "origin/$BRANCH:$MARKER2" 2>/dev/null \
    || die "'$BRANCH' unexpectedly contains $MARKER2 (branches not isolated)"
  ok "branch contents differ as expected ($BRANCH2 has $MARKER2, $BRANCH does not)"

  # === 9. Git LFS: store a large binary in the bucket and round-trip it =====
  if command -v git-lfs >/dev/null 2>&1; then
    step "Git LFS: pushing a 1 MiB object → $STORE"
    (
      cd "$FRESH"
      git lfs install --local >/dev/null
      git config lfs.locksverify false   # silence the lock-verify hint (it echoes the token-bearing URL)
      git lfs track "*.bin" >/dev/null
      head -c 1048576 /dev/urandom > big.bin
      cp big.bin "$WORK/big.bin.orig"
      git -c user.email='e2e@bucketvcs.test' -c user.name='bucketvcs e2e' add .gitattributes big.bin
      git -c user.email='e2e@bucketvcs.test' -c user.name='bucketvcs e2e' \
        commit --quiet -m "e2e: add LFS object big.bin"
      GIT_TERMINAL_PROMPT=0 git push --quiet origin "$BRANCH"
    )
    git -C "$FRESH" cat-file -p "HEAD:big.bin" | head -1 | grep -q '^version https://git-lfs' \
      || die "big.bin in Git is not an LFS pointer — the LFS clean filter did not engage"
    ok "Git stores an LFS pointer; 1 MiB blob pushed to $STORE"

    step "Git LFS: fresh clone + 'git lfs pull', verify bytes"
    LFSCLONE="$WORK/lfsclone"
    GIT_LFS_SKIP_SMUDGE=1 git_ni clone --quiet "$REMOTE" "$LFSCLONE"
    ( cd "$LFSCLONE"; git lfs install --local >/dev/null; GIT_TERMINAL_PROMPT=0 git lfs pull )
    [ -f "$LFSCLONE/big.bin" ] || die "LFS object big.bin missing after 'git lfs pull'"
    cmp "$WORK/big.bin.orig" "$LFSCLONE/big.bin" || die "LFS round-trip byte mismatch"
    ok "LFS object round-tripped through $STORE byte-for-byte (1 MiB)"

    # Direct-mode markers: the Batch API ran (lfs.batch), but the gateway never
    # proxied the blob bytes (lfs.object.served absent) → the client transferred
    # straight to/from the bucket via a presigned URL. If lfs.object.served fired,
    # direct mode silently degraded to the gateway proxy — the regression this
    # check (ported from the m13 smokes) exists to catch.
    grep -q 'lfs\.batch' "$SERVE_LOG" || die "LFS: expected an lfs.batch audit event (Batch API not exercised)"
    if grep -q 'lfs\.object\.served' "$SERVE_LOG"; then
      die "LFS did not go direct: the gateway proxied the object (lfs.object.served fired); expected a presigned direct transfer for $BACKEND"
    fi
    ok "LFS used the direct presigned-URL path (lfs.batch fired, lfs.object.served did not)"
  else
    step "Git LFS"
    info "git-lfs not on PATH — skipping the LFS phase (install from https://git-lfs.com/)"
  fi

  # === 10. Security: a read-only token must NOT be able to push =============
  step "Security: a repo:read-only token must NOT be able to push"
  RO_TOKEN="$("$BVCS" token create "$USER_NAME" --auth-db "$DB" --scopes=repo:read --label=e2e-ro | sed -n 's/^token=//p')"
  [ -n "$RO_TOKEN" ] || die "failed to mint a read-only token"
  if git_ni -C "$FRESH" push "http://$USER_NAME:$RO_TOKEN@$ADDR/$SLUG.git" \
       "HEAD:refs/heads/scope-denied-$$" >/dev/null 2>&1; then
    die "SECURITY REGRESSION: a repo:read-only token was allowed to push"
  fi
  ok "read-only token push was correctly rejected (scope enforcement holds)"

  # === 11. Policy: deletion of a protected ref must be rejected =============
  step "Policy: deleting a protected ref must be rejected"
  "$BVCS" policy refs add --auth-db "$DB" --tenant "$TENANT" --repo "$REPO" \
    --pattern "refs/heads/$BRANCH2" >/dev/null
  if git_ni -C "$FRESH" push "$REMOTE" --delete "$BRANCH2" >/dev/null 2>&1; then
    die "POLICY REGRESSION: deletion of protected ref '$BRANCH2' was allowed"
  fi
  git_ni ls-remote "$REMOTE" | grep -q "refs/heads/$BRANCH2" \
    || die "protected ref '$BRANCH2' vanished despite the rejection"
  ok "protected-ref deletion was correctly rejected and '$BRANCH2' survived"

  PASSED=1
  step "PASS"
  printf '%s  bucketvcs e2e (%s) succeeded: %s pushed through %s and verified.%s\n' \
    "$GRN" "$BACKEND" "$SRC_REPO" "$STORE" "$RST"
}
