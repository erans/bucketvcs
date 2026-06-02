#!/usr/bin/env bash
# scripts/web-ui-smoke-localfs.sh
#
# End-to-end smoke test for the web UI against a localfs gateway. Drives
# curl through the login flow, verifies CSRF double-submit enforcement, bad
# credentials rejection, session-cookie auth on the landing page, and that
# the git dispatcher still serves info/refs for a public repo without the UI
# swallowing the request.
#
# Requires no cloud credentials — the local counterpart to the cloud e2e suite.
# Dependencies: curl, go, openssl (for port selection via python3 or fallback).

set -euo pipefail

ROOT="$(mktemp -d)"
echo "smoke root: $ROOT"

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== SMOKE FAILED (status=$cleanup_status) ===="
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
    echo "ALL WEB UI SMOKE CHECKS PASSED"
}
trap cleanup EXIT

# 1. Build bucketvcs.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$ROOT/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

# 2. Set up auth DB, store, and a local user.
STORE_DIR="$ROOT/store"
mkdir -p "$STORE_DIR"
AUTH_DB="$ROOT/auth.db"

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
BASE_URL="http://127.0.0.1:$PORT"

"$BUCKETVCS" user add alice --auth-db "$AUTH_DB"
echo "s3cretpw" | "$BUCKETVCS" user set-password alice --auth-db "$AUTH_DB" --password-stdin

# Register a public repo so the landing page shows a tenant.
# We pass --store so M1 initialises the bucket layout on disk; this allows
# the gateway to serve info/refs (200) rather than 404 for an uninitialised
# store.  A push is NOT required — the authdb row + initialised store are
# enough for both the landing-page listing and the git dispatcher check.
"$BUCKETVCS" repo register acme/demo --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR"
"$BUCKETVCS" repo public acme/demo on --auth-db "$AUTH_DB"

# 3. Launch server (--lfs=false: avoids the hard-require of --proxied-url-signing-key).
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "localfs:$STORE_DIR" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$ROOT/mirror" \
    --lfs=false \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!

# 4. Wait for server ready.
for i in $(seq 1 50); do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "server did not start"; exit 1; }
echo "READY"

JAR="$ROOT/cookies"
JAR_BAD="$ROOT/cookies-bad"

# 5. GET /login — obtain CSRF token from the hidden form field.
echo "== GET /login =="
login_html=$(curl -sS -c "$JAR" "$BASE_URL/login")
csrf=$(printf '%s' "$login_html" | grep -o 'name="csrf_token" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')
test -n "$csrf" || { echo "FAIL: no csrf_token found in login page"; exit 1; }
echo "  csrf=${csrf:0:8}…"

# 6. POST /login with correct credentials — expect 303 redirect to /.
echo "== POST /login (good credentials) =="
login_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=alice" \
    --data-urlencode "password=s3cretpw" \
    --data-urlencode "csrf_token=$csrf" \
    --data-urlencode "next=/" \
    "$BASE_URL/login")
test "$login_code" = "303" || { echo "FAIL: login returned $login_code (want 303)"; exit 1; }
echo "  -> $login_code OK"

# 7. GET / with session cookie — landing page must mention the tenant.
echo "== GET / (authed) shows tenant =="
landing=$(curl -sS -b "$JAR" "$BASE_URL/")
printf '%s' "$landing" | grep -q "acme" || { echo "FAIL: landing page missing tenant 'acme'"; exit 1; }
echo "  tenant 'acme' present OK"

# 8. Bad-password login must return 401.
echo "== POST /login (bad credentials) =="
csrf2=$(curl -sS -c "$JAR_BAD" "$BASE_URL/login" | grep -o 'name="csrf_token" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')
test -n "$csrf2" || { echo "FAIL: no csrf token for bad-login attempt"; exit 1; }
bad_code=$(curl -sS -b "$JAR_BAD" -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=alice" \
    --data-urlencode "password=WRONG" \
    --data-urlencode "csrf_token=$csrf2" \
    "$BASE_URL/login")
test "$bad_code" = "401" || { echo "FAIL: bad-login returned $bad_code (want 401)"; exit 1; }
echo "  -> $bad_code OK"

# 9. Missing CSRF token must return 403.
echo "== POST /login (no CSRF) =="
nocsrf_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=alice" \
    --data-urlencode "password=s3cretpw" \
    "$BASE_URL/login")
test "$nocsrf_code" = "403" || { echo "FAIL: missing-csrf returned $nocsrf_code (want 403)"; exit 1; }
echo "  -> $nocsrf_code OK"

# 10. Git dispatcher must still serve public-repo info/refs (not swallowed by UI).
echo "== git info/refs for public repo =="
inforefs_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    "$BASE_URL/acme/demo.git/info/refs?service=git-upload-pack")
echo "  info/refs -> $inforefs_code"
# An initialised but empty localfs repo returns 200 with the capability
# advertisement (no objects, but the upload-pack service line is valid).
# If the dispatcher had mis-routed this to the UI, the landing handler
# would reject the non-/ path with 404.
test "$inforefs_code" = "200" || { echo "FAIL: info/refs returned $inforefs_code (want 200)"; exit 1; }
echo "  dispatcher routes git correctly OK"

# Cleanup on exit (trap) prints "ALL WEB UI SMOKE CHECKS PASSED".
