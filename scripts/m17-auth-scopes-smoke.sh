#!/usr/bin/env bash
# scripts/m17-auth-scopes-smoke.sh
#
# End-to-end smoke for M17 token scopes + rotation against localfs:
#   1. Build bucketvcs; init a repo + authdb.
#   2. Create user 'alice' with write grant. Issue a repo:read scoped token.
#   3. Clone over HTTPS Basic-auth — must succeed (repo:read covers fetch).
#   4. Push from that clone — must be rejected 403 "insufficient scope".
#   5. Rotate the read token; verify the new secret differs from the old.
#   6. Clone with the new (rotated) secret — must succeed.
#   7. Clone with the old (rotated-out) secret — must fail.
#   8. Issue a repo:write scoped token; push from clone — must succeed.
#   9. Confirm `auth.scope.denied` audit landed in the gateway log.
#
# Exits with `M17_AUTH_SCOPES_SMOKE_OK` on success. Skips with exit 77 if
# go / git is missing.

set -euo pipefail

if ! command -v go  >/dev/null 2>&1; then echo "SKIP: go not on PATH";  exit 77; fi
if ! command -v git >/dev/null 2>&1; then echo "SKIP: git not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE="localfs:$ROOT/store"
AUTHDB="$ROOT/auth.db"
TENANT="acme"
REPO="m17smoke"
PORT="$(awk 'BEGIN{srand(); print 30000+int(rand()*10000)}')"
URL="http://127.0.0.1:$PORT"

PID=""
cleanup() {
    rc=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M17_AUTH_SCOPES_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT; gateway.log + push/clone .err files)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init + register repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO"
"$BIN" repo register "$TENANT/$REPO" --auth-db="$AUTHDB" --store="$STORE" --no-init

echo "==> Create user + grant write"
"$BIN" user add alice --auth-db="$AUTHDB"
"$BIN" repo grant alice "$TENANT/$REPO" write --auth-db="$AUTHDB"

echo "==> Issue repo:read scoped token"
"$BIN" token create alice --auth-db="$AUTHDB" --scopes=repo:read \
    >"$ROOT/read-token.out" 2>"$ROOT/read-token.err"
READ_ID=$(grep -m1 '^id=' "$ROOT/read-token.out" | sed 's/^id=//')
READ_TOKEN=$(grep -m1 '^token=' "$ROOT/read-token.out" | sed 's/^token=//')
if [[ -z "$READ_ID" || -z "$READ_TOKEN" ]]; then
    echo "FAIL: read token output missing id/token"
    echo "--- stdout ---"; cat "$ROOT/read-token.out"
    echo "--- stderr ---"; cat "$ROOT/read-token.err"
    exit 1
fi
if ! grep -q '^scopes=repo:read$' "$ROOT/read-token.out"; then
    echo "FAIL: read token output missing scopes=repo:read"
    cat "$ROOT/read-token.out"
    exit 1
fi
echo "    read token id=$READ_ID"

echo "==> Start gateway on $URL"
"$BIN" serve --store="$STORE" --auth-db="$AUTHDB" --addr="127.0.0.1:$PORT" --lfs=false \
    --mirror-dir="$ROOT/mirror" \
    >"$ROOT/gateway.log" 2>&1 &
PID=$!
for i in $(seq 1 50); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "FAIL: gateway died early"
        cat "$ROOT/gateway.log"
        exit 1
    fi
    sleep 0.2
done

READ_URL="http://alice:$READ_TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

echo "==> Step 1: clone with repo:read"
git clone -q "$READ_URL" "$ROOT/clone1" 2>"$ROOT/clone1.err" || {
    echo "FAIL: clone with repo:read failed"
    echo "--- clone1.err ---"; cat "$ROOT/clone1.err"
    echo "--- gateway.log tail ---"; tail -40 "$ROOT/gateway.log"
    exit 1
}
echo "    clone with repo:read succeeded"

echo "==> Step 2: push with repo:read (must be rejected)"
(
    cd "$ROOT/clone1"
    git config user.email smoke@local
    git config user.name smoke
    git checkout -q -b main 2>/dev/null || git checkout -q main 2>/dev/null || true
    git commit -q --allow-empty -m "rd-push"
    if git push -q "$READ_URL" HEAD:refs/heads/main 2>"$ROOT/push-read.err"; then
        echo "FAIL: push with repo:read unexpectedly succeeded"
        exit 1
    fi
)
if ! grep -Eq 'insufficient scope|repo:write|403' "$ROOT/push-read.err"; then
    echo "FAIL: push reject error missing 'insufficient scope' marker"
    echo "--- push-read.err ---"; cat "$ROOT/push-read.err"
    exit 1
fi
echo "    push with repo:read rejected as expected"

# Round-1 roborev M1 — info/refs is gated by the same scope as the
# corresponding POST. Verify by issuing an lfs:read-only token whose user has
# read perm on the repo: ls-remote (which calls GET /info/refs?service=
# git-upload-pack) must be rejected because the token lacks repo:read.
echo "==> Step 2.5: ls-remote with lfs:read-only token (must be rejected)"
"$BIN" token create alice --auth-db="$AUTHDB" --scopes=lfs:read \
    >"$ROOT/lfs-only-token.out" 2>"$ROOT/lfs-only-token.err"
LFSONLY_TOKEN=$(grep -m1 '^token=' "$ROOT/lfs-only-token.out" | sed 's/^token=//')
if [[ -z "$LFSONLY_TOKEN" ]]; then
    echo "FAIL: lfs-only token output missing token="
    echo "--- stdout ---"; cat "$ROOT/lfs-only-token.out"
    echo "--- stderr ---"; cat "$ROOT/lfs-only-token.err"
    exit 1
fi
LFSONLY_URL="http://alice:$LFSONLY_TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"
if git ls-remote -q "$LFSONLY_URL" >/dev/null 2>"$ROOT/ls-remote-lfs.err"; then
    echo "FAIL: ls-remote with lfs:read-only token unexpectedly succeeded"
    cat "$ROOT/ls-remote-lfs.err"
    exit 1
fi
if ! grep -Eq 'insufficient scope|repo:read|403' "$ROOT/ls-remote-lfs.err"; then
    echo "FAIL: ls-remote rejection missing insufficient-scope marker"
    echo "--- ls-remote-lfs.err ---"; cat "$ROOT/ls-remote-lfs.err"
    exit 1
fi
echo "    ls-remote with lfs:read-only token rejected as expected"

echo "==> Step 3: rotate read token"
"$BIN" token rotate --auth-db="$AUTHDB" --id="$READ_ID" \
    >"$ROOT/rotate.out" 2>"$ROOT/rotate.err"
NEW_TOKEN=$(grep -m1 '^token=' "$ROOT/rotate.out" | sed 's/^token=//')
if [[ -z "$NEW_TOKEN" ]]; then
    echo "FAIL: rotate output missing token"
    echo "--- stdout ---"; cat "$ROOT/rotate.out"
    echo "--- stderr ---"; cat "$ROOT/rotate.err"
    exit 1
fi
if [[ "$NEW_TOKEN" == "$READ_TOKEN" ]]; then
    echo "FAIL: rotate produced same secret as before"
    exit 1
fi
if ! grep -q "rotated" "$ROOT/rotate.out"; then
    echo "FAIL: rotate output missing 'rotated' marker"
    cat "$ROOT/rotate.out"
    exit 1
fi
echo "    rotate produced a fresh secret"

NEW_URL="http://alice:$NEW_TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

echo "==> Step 4: clone with rotated (new) secret"
git clone -q "$NEW_URL" "$ROOT/clone2" 2>"$ROOT/clone2.err" || {
    echo "FAIL: clone with rotated secret failed"
    echo "--- clone2.err ---"; cat "$ROOT/clone2.err"
    echo "--- gateway.log tail ---"; tail -40 "$ROOT/gateway.log"
    exit 1
}
echo "    clone with rotated secret succeeded"

echo "==> Step 5: clone with rotated-out (old) secret must fail"
if git clone -q "$READ_URL" "$ROOT/clone-old" 2>"$ROOT/clone-old.err"; then
    echo "FAIL: clone with rotated-out secret unexpectedly succeeded"
    exit 1
fi
echo "    clone with rotated-out secret rejected as expected"

echo "==> Step 6: issue repo:write scoped token"
"$BIN" token create alice --auth-db="$AUTHDB" --scopes=repo:write \
    >"$ROOT/write-token.out" 2>"$ROOT/write-token.err"
WRITE_TOKEN=$(grep -m1 '^token=' "$ROOT/write-token.out" | sed 's/^token=//')
if [[ -z "$WRITE_TOKEN" ]]; then
    echo "FAIL: write token output missing token"
    cat "$ROOT/write-token.out"
    exit 1
fi
if ! grep -q '^scopes=repo:write$' "$ROOT/write-token.out"; then
    echo "FAIL: write token output missing scopes=repo:write"
    cat "$ROOT/write-token.out"
    exit 1
fi
echo "    write token issued"

WRITE_URL="http://alice:$WRITE_TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

echo "==> Step 7: push with repo:write must succeed"
(
    cd "$ROOT/clone2"
    git config user.email smoke@local
    git config user.name smoke
    git checkout -q -b main 2>/dev/null || git checkout -q main 2>/dev/null || true
    git commit -q --allow-empty -m "wr-push"
    git push -q "$WRITE_URL" HEAD:refs/heads/main 2>"$ROOT/push-write.err" || {
        echo "FAIL: push with repo:write failed"
        echo "--- push-write.err ---"; cat "$ROOT/push-write.err"
        echo "--- gateway.log tail ---"; tail -40 "$ROOT/gateway.log"
        exit 1
    }
)
echo "    push with repo:write succeeded"

echo "==> Step 8: confirm auth.scope.denied audit landed in gateway log"
# Give the gateway a tick to flush slog default for the receive-pack denial.
sleep 0.5
if ! grep -q "auth.scope.denied" "$ROOT/gateway.log"; then
    echo "FAIL: no auth.scope.denied entry in gateway log"
    echo "--- gateway.log tail ---"; tail -80 "$ROOT/gateway.log"
    exit 1
fi
echo "    audit observed"

echo "==> M17 auth scopes smoke: OK"
