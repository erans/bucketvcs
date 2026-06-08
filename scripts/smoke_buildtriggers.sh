#!/usr/bin/env bash
# scripts/smoke_buildtriggers.sh
#
# End-to-end smoke test for M30 build triggers against a localfs gateway.
# Mirrors scripts/lfs-smoke-localfs.sh's harness (build binary, init localfs
# repo + user + push token, start `bucketvcs serve` in the background, wait on
# /healthz, push over HTTPS with a token, trap-based cleanup).
#
# What it proves:
#   - A push to a ref matching a generic trigger's ref-include fires a delivery.
#   - The delivered POST body carries the repo name and an injected short-lived
#     bvts_ token (token-mode=inject).
#   - That injected token clones the trigger's own repo (acme/app) but is
#     scope-bound: cloning acme/other with it is rejected (single-repo scoping).
#   - A push to a NON-matching ref (refs/heads/dev) fires NO delivery.
#   - The delivery worker marks the row `delivered`.
#
# Needs no cloud credentials. Requires `go`, `git`, `python3`, `curl`.
#
# Known race (inherited from the LFS smoke): we pick a free port via a
# transient Python bind+close, then ask the server to bind it. Another
# process can grab it in the gap. Symptom is a READY failure; rerun.

set -euo pipefail

for tool in go git python3 curl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "SKIP: $tool not on PATH"; exit 77; }
done

ROOT="$(mktemp -d)"
echo "smoke root: $ROOT"

SERVE_PID=""
RECV_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== SMOKE FAILED (status=$cleanup_status) ===="
        echo "Preserved root for forensics: $ROOT"
        if [[ -f "$ROOT/serve.log" ]]; then
            echo "==== serve.log (last 60 lines) ===="
            tail -60 "$ROOT/serve.log" || true
        fi
        if [[ -f "$ROOT/recv.log" ]]; then
            echo "==== recv.log (last 20 lines) ===="
            tail -20 "$ROOT/recv.log" || true
        fi
    fi
    [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
    [[ -n "$RECV_PID" ]] && kill "$RECV_PID" 2>/dev/null || true
    if [[ $cleanup_status -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "BUILDTRIGGERS_LOCALFS_SMOKE_OK"
    fi
}
trap cleanup EXIT

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; exit 1; }

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# 1. Build bucketvcs.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$ROOT/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )
pass "1. built bucketvcs"

# 2. Auth + repos.
STORE_DIR="$ROOT/store"; mkdir -p "$STORE_DIR"
AUTH_DB="$ROOT/auth.db"

GIT_PORT="$(free_port)"
RECV_PORT="$(free_port)"
[[ "$GIT_PORT" != "$RECV_PORT" ]] || fail "git and receiver ports collided ($GIT_PORT)"
GIT_BASE="http://127.0.0.1:$GIT_PORT"

"$BUCKETVCS" user add alice --auth-db "$AUTH_DB"
TOKEN=$("$BUCKETVCS" token create alice --auth-db "$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
[[ -n "$TOKEN" ]] || fail "could not extract push token for alice"
"$BUCKETVCS" repo register acme/app   --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR"
"$BUCKETVCS" repo grant alice acme/app write --auth-db "$AUTH_DB"
"$BUCKETVCS" repo register acme/other --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR"
"$BUCKETVCS" repo grant alice acme/other write --auth-db "$AUTH_DB"
pass "2. seeded user, push token, repos acme/app + acme/other"

# 3. Tiny HTTP receiver: append each request body as one line to BODY_FILE,
#    reply 200. One line per delivery so we can count deliveries precisely.
BODY_FILE="$ROOT/deliveries.ndjson"
: > "$BODY_FILE"
RECV_PY="$ROOT/recv.py"
cat > "$RECV_PY" <<'PYEOF'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
BODY_FILE = sys.argv[2]

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n) if n else b""
        with open(BODY_FILE, "ab") as f:
            f.write(body)
            f.write(b"\n")
            f.flush()
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *a):
        pass

HTTPServer(("127.0.0.1", PORT), H).serve_forever()
PYEOF
python3 "$RECV_PY" "$RECV_PORT" "$BODY_FILE" > "$ROOT/recv.log" 2>&1 &
RECV_PID=$!
# Wait for the receiver to accept connections.
for _ in $(seq 1 50); do
    if curl -s -o /dev/null "http://127.0.0.1:$RECV_PORT/" 2>/dev/null; then break; fi
    sleep 0.1
done
curl -s -o /dev/null "http://127.0.0.1:$RECV_PORT/" || fail "receiver did not start"
pass "3. receiver listening on 127.0.0.1:$RECV_PORT"

# 4. Start serve with build triggers + loopback egress allowed.
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$GIT_PORT" \
    --store "localfs:$STORE_DIR" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$ROOT/mirror" \
    --lfs=false \
    --build-triggers \
    --webhook-allow-cidr 127.0.0.1/32 \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 50); do
    if curl -sf "$GIT_BASE/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.1
done
curl -sf "$GIT_BASE/healthz" >/dev/null || fail "server did not start"
pass "4. serve ready on $GIT_BASE (build triggers on)"

# 5. Register a generic trigger on acme/app, ref-include refs/heads/main,
#    token-mode=inject so the body carries a single-repo read token.
"$BUCKETVCS" build trigger add \
    --auth-db "$AUTH_DB" \
    --tenant acme --repo app \
    --name main --kind generic \
    --token-mode inject \
    --url "http://127.0.0.1:$RECV_PORT/" \
    --ref-include refs/heads/main
pass "5. registered generic trigger main (ref-include refs/heads/main, token inject)"

# 6. Push a commit to refs/heads/main over HTTPS.
REPO="$ROOT/repo"; mkdir -p "$REPO"
(
    cd "$REPO"
    git init -q -b main
    git config user.email smoke@example.com
    git config user.name smoke
    echo "hello build triggers" > README.md
    git add README.md
    git commit -qm "initial commit"
    git remote add origin "http://alice:$TOKEN@127.0.0.1:$GIT_PORT/acme/app.git"
    git push -q origin main
)
pass "6. pushed commit to refs/heads/main"

# 7. Poll the receiver file (~15s) for a body; assert repo + bvts_ token.
DELIVERED=""
for _ in $(seq 1 75); do
    if [[ -s "$BODY_FILE" ]]; then DELIVERED="$(head -1 "$BODY_FILE")"; break; fi
    sleep 0.2
done
[[ -n "$DELIVERED" ]] || fail "no delivery body arrived within ~15s"
echo "$DELIVERED" | grep -q '"repo":"app"' || fail "body missing \"repo\":\"app\": $DELIVERED"
echo "$DELIVERED" | grep -q '"bvts_token":"bvts_' || fail "body missing injected bvts_ token: $DELIVERED"
pass "7. delivery body carries repo=app + injected bvts_ token"

# 8. Extract the injected token and prove single-repo scoping.
INJECTED=$(echo "$DELIVERED" | python3 -c 'import sys,json; print(json.load(sys.stdin)["bvts_token"])')
[[ -n "$INJECTED" ]] || fail "could not extract injected token from body"
git clone -q "http://x-access-token:$INJECTED@127.0.0.1:$GIT_PORT/acme/app.git" "$ROOT/cloned_app" \
    || fail "injected token failed to clone its own repo acme/app"
test -s "$ROOT/cloned_app/README.md" || fail "cloned acme/app is missing README.md"
pass "8a. injected token clones acme/app"

if git clone -q "http://x-access-token:$INJECTED@127.0.0.1:$GIT_PORT/acme/other.git" "$ROOT/cloned_other" 2>/dev/null; then
    fail "injected token cloned acme/other — single-repo scoping is broken"
fi
pass "8b. injected token rejected for acme/other (single-repo scoping)"

# 9. Push to a NON-matching ref; assert NO new delivery arrives.
BEFORE_COUNT=$(wc -l < "$BODY_FILE")
(
    cd "$REPO"
    git checkout -q -b dev
    echo "dev work" >> README.md
    git commit -qam "dev commit"
    git push -q origin dev
)
sleep 3
AFTER_COUNT=$(wc -l < "$BODY_FILE")
[[ "$AFTER_COUNT" -eq "$BEFORE_COUNT" ]] \
    || fail "non-matching ref refs/heads/dev produced a delivery (before=$BEFORE_COUNT after=$AFTER_COUNT)"
pass "9. push to refs/heads/dev produced no delivery (count stayed $BEFORE_COUNT)"

# 10. delivery list shows a delivered row (poll until worker marks it).
DELIVERED_ROW=""
for _ in $(seq 1 75); do
    if "$BUCKETVCS" build delivery list --auth-db "$AUTH_DB" --status delivered 2>/dev/null | grep -q .; then
        DELIVERED_ROW="$("$BUCKETVCS" build delivery list --auth-db "$AUTH_DB" --status delivered)"
        break
    fi
    sleep 0.2
done
[[ -n "$DELIVERED_ROW" ]] || fail "no delivered delivery row within ~15s"
echo "$DELIVERED_ROW" | grep -qi 'delivered' || fail "delivery list row not in delivered state: $DELIVERED_ROW"
pass "10. build delivery list shows a delivered row"

echo "ALL BUILD-TRIGGER SMOKE CHECKS PASSED"
