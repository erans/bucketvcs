#!/usr/bin/env bash
# scripts/m20-hooks-smoke.sh
#
# End-to-end smoke for M20 Tier 3 custom hooks against localfs.
#
# WHY THE SEQUENCE LOOKS A LITTLE ODD: there is a pre-existing receivepack
# behavior whereby a successful push followed by a push to a DIFFERENT branch
# from an unrelated tip can fail with HTTP 500 in this single-process serve
# setup. The bug predates M20 (reproducible with --hooks-enabled=false) and is
# unrelated to hook plumbing. To exercise the M20 hook surface fully without
# tripping that landmine, the smoke uses two phases:
#
#   Phase A (no state mutation server-side):
#     Push refs/heads/forbidden -> hook rejects -> assert client stderr
#     "is not allowed" and policy.hook.rejected audit. The rejection short-
#     circuits before BuildAndCommit so no server state changes.
#
#   Phase B (after CLI-disable, state mutation allowed):
#     Disable the reject hook. Push refs/heads/forbidden again -> ACCEPT.
#     Assert the post-receive marker fires (audit-marker.sh touches a file).
#
# The two phases together cover: pre-receive reject + client-visible stderr
# propagation + audit emission + CLI disable flip + post-receive worker drain.
#
# Exits with `M20_HOOKS_SMOKE_OK` on success. Skips with exit 77 if go/git/curl
# are missing.

set -euo pipefail

if ! command -v go   >/dev/null 2>&1; then echo "SKIP: go not on PATH";   exit 77; fi
if ! command -v git  >/dev/null 2>&1; then echo "SKIP: git not on PATH";  exit 77; fi
if ! command -v curl >/dev/null 2>&1; then echo "SKIP: curl not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

TMPDIR="$(mktemp -d)"
SERVE_LOG="$TMPDIR/serve.log"
STORE_DIR="$TMPDIR/store"
STORE="localfs:$STORE_DIR"
AUTH_DB="$TMPDIR/auth.db"
HOOKS_ROOT="$TMPDIR/hooks"
MARKER="$TMPDIR/post-receive-ran"
TENANT="acme"
REPO="r1"
mkdir -p "$STORE_DIR" "$HOOKS_ROOT"

# Pick a free port: random pick + collision retry (M19 pattern).
pick_port() {
    local i candidate inuse
    inuse="$(ss -ltn 2>/dev/null | awk 'NR>1 {sub(/.*:/, "", $4); print $4}' | sort -u)"
    for i in $(seq 1 20); do
        candidate="$(awk 'BEGIN{srand('"$$$i"'); print 30000+int(rand()*10000)}')"
        if ! grep -qx "$candidate" <<<"$inuse"; then
            echo "$candidate"
            return 0
        fi
    done
    echo "FAIL: could not find free port after 20 attempts" >&2
    return 1
}
PORT="$(pick_port)"
URL="http://127.0.0.1:$PORT"

SERVE_PID=""
KEEP_TMP="${KEEP_TMP:-0}"
cleanup() {
    rc=$?
    if [[ -n "$SERVE_PID" ]] && kill -0 "$SERVE_PID" 2>/dev/null; then
        kill "$SERVE_PID" 2>/dev/null || true
        wait "$SERVE_PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 && "$KEEP_TMP" != "1" ]]; then
        rm -rf "$TMPDIR"
        echo "M20_HOOKS_SMOKE_OK"
    else
        echo "(forensics preserved at $TMPDIR; serve.log inside)" >&2
        echo "--- last 80 lines of serve.log ---" >&2
        tail -80 "$SERVE_LOG" 2>/dev/null >&2 || true
    fi
    rm -f "$BIN"
    exit "$rc"
}
on_failure() { KEEP_TMP=1; }
trap on_failure ERR
trap cleanup EXIT

# Pre-receive rejecter (only rejects refs/heads/forbidden).
cat >"$HOOKS_ROOT/reject-forbidden.sh" <<'EOF'
#!/bin/sh
while read -r oldoid newoid refname; do
    case "$refname" in
        refs/heads/forbidden) echo "rejected: refs/heads/forbidden is not allowed" >&2; exit 1 ;;
    esac
done
exit 0
EOF
chmod +x "$HOOKS_ROOT/reject-forbidden.sh"

# Post-receive marker (touches a file).
cat >"$HOOKS_ROOT/audit-marker.sh" <<EOF
#!/bin/sh
touch "$MARKER"
exit 0
EOF
chmod +x "$HOOKS_ROOT/audit-marker.sh"

echo "==> Init + register repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO"
"$BIN" repo register "$TENANT/$REPO" --auth-db="$AUTH_DB" --store="$STORE" --no-init

echo "==> Create user + token + grant"
"$BIN" user add alice --auth-db="$AUTH_DB"
"$BIN" repo grant alice "$TENANT/$REPO" write --auth-db="$AUTH_DB"
TOKEN=$("$BIN" token create alice --auth-db="$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
if [[ -z "$TOKEN" ]]; then echo "FAIL: could not extract alice token"; exit 1; fi

echo "==> Register hooks via CLI"
"$BIN" policy hooks add --auth-db="$AUTH_DB" \
    --tenant="$TENANT" --repo="$REPO" --trigger=pre-receive --script=reject-forbidden.sh
"$BIN" policy hooks add --auth-db="$AUTH_DB" \
    --tenant="$TENANT" --repo="$REPO" --trigger=post-receive --script=audit-marker.sh

# Detect bwrap availability AND --rlimit-cpu/--rlimit-as support — the runner
# always passes those flags in sandbox mode, and bwrap < 0.12 rejects them
# with "Unknown option --rlimit-cpu". Fall back to unsafe-no-sandbox if either
# the binary is missing or the build doesn't expose the rlimit flags. (Some
# dev hosts ship 0.11.x without rlimit support; M20 Task 9 exercises the real
# bwrap path under integration tests, not this smoke.)
SANDBOX_FLAGS=(--hooks-unsafe-no-sandbox=true)
SANDBOX_MODE="unsafe-no-sandbox"
if command -v bwrap >/dev/null 2>&1; then
    if bwrap --rlimit-cpu 1 -- /bin/true >/dev/null 2>&1; then
        SANDBOX_FLAGS=()
        SANDBOX_MODE="sandbox"
    else
        echo "==> bwrap present but lacks --rlimit-cpu (need >= 0.12); falling back to --hooks-unsafe-no-sandbox"
    fi
else
    echo "==> bwrap not available, running with --hooks-unsafe-no-sandbox"
fi
echo "==> sandbox mode: $SANDBOX_MODE"

echo "==> Start gateway on $URL"
"$BIN" serve \
    --store="$STORE" \
    --auth-db="$AUTH_DB" \
    --addr="127.0.0.1:$PORT" \
    --lfs=false \
    --mirror-dir="$TMPDIR/mirror" \
    --hooks-enabled=true \
    --hooks-root="$HOOKS_ROOT" \
    "${SANDBOX_FLAGS[@]}" \
    >"$SERVE_LOG" 2>&1 &
SERVE_PID=$!

for i in $(seq 1 50); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$SERVE_PID" 2>/dev/null; then echo "FAIL: gateway died early"; cat "$SERVE_LOG"; exit 1; fi
    sleep 0.2
done
if ! curl -sf "$URL/healthz" >/dev/null 2>&1; then
    echo "FAIL: gateway never came up"
    exit 1
fi

CLONE_URL="http://alice:$TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

# seed_repo populates $1 with a single-commit history on branch $2.
seed_repo() {
    local WD="$1" BRANCH="$2"
    rm -rf "$WD"; mkdir -p "$WD"
    (
        cd "$WD"
        git init -q -b "$BRANCH"
        git config user.email m20@example.com
        git config user.name m20
        git config commit.gpgsign false
        echo "hello $BRANCH" > README.md
        git add README.md
        git commit -q -m "seed-$BRANCH"
    )
}

# Phase A: push refs/heads/forbidden, expect hook reject + client-visible
# stderr. The reject short-circuits before BuildAndCommit so server state
# is unchanged — important for the next push to not trip the second-push
# 500 landmine.
echo "==> Phase A: push refs/heads/forbidden (expect REJECT + client-visible stderr)"
seed_repo "$TMPDIR/seed-A" forbidden
(
    cd "$TMPDIR/seed-A"
    if git push "$CLONE_URL" HEAD:refs/heads/forbidden 2>"$TMPDIR/push-err-A"; then
        echo "FAIL: forbidden push should have been rejected"
        cat "$TMPDIR/push-err-A"
        exit 1
    fi
)
grep -q "is not allowed" "$TMPDIR/push-err-A" \
    || { echo "FAIL: client stderr missing 'is not allowed' reject message"; cat "$TMPDIR/push-err-A"; exit 1; }
echo "    forbidden push rejected with client-visible 'is not allowed'"

echo "==> Verify policy.hook.rejected audit in serve.log"
grep -q "policy.hook.rejected" "$SERVE_LOG" \
    || { echo "FAIL: serve.log missing policy.hook.rejected audit"; tail -50 "$SERVE_LOG" >&2; exit 1; }
echo "    audit event present"

# Sanity: the marker MUST NOT exist yet — no successful push has happened
# (post-receive only runs after Step 14 of receive-pack, which a rejected
# push never reaches).
[[ ! -f "$MARKER" ]] || { echo "FAIL: marker exists before any successful push"; exit 1; }
echo "    post-receive marker correctly absent before any accepted push"

# Phase B: disable the reject hook + re-push refs/heads/forbidden, expect
# ACCEPT. This is the first state-mutating push of the smoke, so the
# post-receive marker fires here.
echo "==> Phase B: disable reject hook + push refs/heads/forbidden (expect ACCEPT + marker)"
"$BIN" policy hooks disable --auth-db="$AUTH_DB" \
    --tenant="$TENANT" --repo="$REPO" --trigger=pre-receive --script=reject-forbidden.sh

seed_repo "$TMPDIR/seed-B" forbidden
(
    cd "$TMPDIR/seed-B"
    git push -q "$CLONE_URL" HEAD:refs/heads/forbidden 2>"$TMPDIR/push-err-B" \
        || { echo "FAIL: forbidden push should have succeeded after disable"; cat "$TMPDIR/push-err-B"; exit 1; }
)
echo "    forbidden push accepted after hook disabled"

# Post-receive worker drains async; poll up to 5s.
for i in $(seq 1 50); do
    if [[ -f "$MARKER" ]]; then break; fi
    sleep 0.1
done
[[ -f "$MARKER" ]] || { echo "FAIL: post-receive marker not created"; tail -30 "$SERVE_LOG" >&2; exit 1; }
echo "    post-receive marker present"

echo
echo "All M20 custom-hooks assertions passed."
