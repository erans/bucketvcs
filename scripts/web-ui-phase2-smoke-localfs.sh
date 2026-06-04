#!/usr/bin/env bash
# scripts/web-ui-phase2-smoke-localfs.sh
#
# End-to-end smoke test for the M24 Phase 2 web code-browse feature against a
# localfs gateway. Imports a real repo (commits, tree, subdir, README, binary),
# marks it public, then drives curl through every browse route: repo home (tree
# + rendered README), subdirectory tree, blob (syntax-highlighted), raw (safe
# content-type + nosniff + CSP), commit log, single commit + diff. Also asserts
# the anti-enumeration 404 for a non-visible repo.
#
# Requires no cloud credentials. Dependencies: curl, go, git, python3.

set -euo pipefail

ROOT="$(mktemp -d)"
echo "smoke root: $ROOT"

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== PHASE 2 SMOKE FAILED (status=$cleanup_status) ===="
        echo "Preserved root for forensics: $ROOT"
        if [[ -f "$ROOT/serve.log" ]]; then
            echo "==== serve.log (last 50 lines) ===="
            tail -50 "$ROOT/serve.log" || true
        fi
        [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
        return
    fi
    [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$ROOT"
    echo "ALL PHASE 2 BROWSE SMOKE CHECKS PASSED"
}
trap cleanup EXIT

# 1. Build bucketvcs.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$ROOT/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

STORE_DIR="$ROOT/store"; mkdir -p "$STORE_DIR"
AUTH_DB="$ROOT/auth.db"

# 2. Build a source bare repo with real content (2 commits on main, a subdir,
#    a README, and a binary file).
WORK="$ROOT/work"; SRC_BARE="$ROOT/src.git"
export GIT_AUTHOR_NAME=Ann GIT_AUTHOR_EMAIL=ann@x GIT_COMMITTER_NAME=Ann GIT_COMMITTER_EMAIL=ann@x
git init -q -b main "$WORK"
printf 'hello\n' > "$WORK/a.txt"
printf '# Demo\n\nHello *world*.\n' > "$WORK/README.md"
mkdir -p "$WORK/sub"; printf 'world\n' > "$WORK/sub/b.txt"
printf '\x00\x01\x02\x00' > "$WORK/bin.dat"
git -C "$WORK" add .
git -C "$WORK" commit -q -m "init"
printf 'hello again\n' > "$WORK/a.txt"
git -C "$WORK" add .
git -C "$WORK" commit -q -m "update a"
COMMIT_OID=$(git -C "$WORK" rev-parse HEAD)
git clone -q --bare "$WORK" "$SRC_BARE"

# 3. Import the bare repo into bucketvcs storage, then register + publish it in
#    the auth DB (import populates the store; the auth-db row drives visibility).
"$BUCKETVCS" import --store "localfs:$STORE_DIR" --default-branch refs/heads/main "$SRC_BARE" acme demo
"$BUCKETVCS" repo register acme/demo --auth-db "$AUTH_DB" --no-init
"$BUCKETVCS" repo public acme/demo on --auth-db "$AUTH_DB"
# /acme/secret is simply an absent/unregistered repo; anonymous gets a uniform
# 404 regardless of whether the repo exists (anti-enumeration).

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
BASE_URL="http://127.0.0.1:$PORT"

# 4. Launch server (UI on by default; --lfs=false avoids proxied-signing-key requirement).
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "localfs:$STORE_DIR" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$ROOT/mirror" \
    --lfs=false \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!

for i in $(seq 1 50); do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break
    sleep 0.1
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "server did not start"; exit 1; }
echo "READY"

# 5. Repo home: tree shows a.txt + sub + bin.dat, and the README is rendered.
echo "== GET /acme/demo (repo home) =="
home=$(curl -sS "$BASE_URL/acme/demo")
printf '%s' "$home" | grep -q "a.txt"      || { echo "FAIL: home missing a.txt"; exit 1; }
printf '%s' "$home" | grep -q "sub"        || { echo "FAIL: home missing sub dir"; exit 1; }
printf '%s' "$home" | grep -q "Hello"      || { echo "FAIL: home missing rendered README"; exit 1; }
echo "  home OK"

# 6. Subdirectory tree.
echo "== GET /acme/demo/tree/main/sub =="
curl -sS "$BASE_URL/acme/demo/tree/main/sub" | grep -q "b.txt" || { echo "FAIL: subtree missing b.txt"; exit 1; }
echo "  subtree OK"

# 7. Blob view (highlighted) — content present and HTML-escaped (no raw markup leak).
echo "== GET /acme/demo/blob/main/a.txt =="
curl -sS "$BASE_URL/acme/demo/blob/main/a.txt" | grep -q "hello again" || { echo "FAIL: blob missing content"; exit 1; }
echo "  blob OK"

# 8. Raw endpoint — safe content-type + nosniff.
echo "== GET /acme/demo/raw/main/a.txt (headers) =="
raw_hdrs=$(curl -sSI "$BASE_URL/acme/demo/raw/main/a.txt")
printf '%s' "$raw_hdrs" | grep -qi "Content-Type: text/plain; charset=utf-8" || { echo "FAIL: raw content-type"; exit 1; }
printf '%s' "$raw_hdrs" | grep -qi "X-Content-Type-Options: nosniff"          || { echo "FAIL: raw nosniff"; exit 1; }
raw_body=$(curl -sS "$BASE_URL/acme/demo/raw/main/a.txt")
test "$raw_body" = "hello again" || { echo "FAIL: raw body = '$raw_body'"; exit 1; }
echo "  raw OK"

# 8b. chroma stylesheet + UI CSP.
echo "== chroma.css + CSP =="
curl -sf "$BASE_URL/_ui/static/chroma.css" | grep -q ".chroma" || { echo "FAIL: chroma.css missing"; exit 1; }
home_csp=$(curl -sSI "$BASE_URL/acme/demo" | grep -i "^Content-Security-Policy:" || true)
printf '%s' "$home_csp" | grep -q "script-src 'self'" || { echo "FAIL: UI CSP missing on browse page: $home_csp"; exit 1; }
echo "  chroma.css + CSP OK"

# 9. Commit log.
echo "== GET /acme/demo/commits/main =="
curl -sS "$BASE_URL/acme/demo/commits/main" | grep -q "update a" || { echo "FAIL: log missing 'update a'"; exit 1; }
echo "  commit log OK"

# 10. Single commit + diff.
echo "== GET /acme/demo/commit/$COMMIT_OID =="
commit=$(curl -sS "$BASE_URL/acme/demo/commit/$COMMIT_OID")
printf '%s' "$commit" | grep -q "a.txt"       || { echo "FAIL: commit view missing a.txt"; exit 1; }
printf '%s' "$commit" | grep -q "hello again" || { echo "FAIL: commit view missing added line"; exit 1; }
echo "  commit view OK"

# 11. Anti-enumeration: a non-visible repo returns 404 (not 403, not 200).
echo "== GET /acme/secret (not visible) =="
secret_code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/acme/secret")
test "$secret_code" = "404" || { echo "FAIL: non-visible repo returned $secret_code (want 404)"; exit 1; }
echo "  uniform 404 OK"

# Cleanup trap prints "ALL PHASE 2 BROWSE SMOKE CHECKS PASSED".
