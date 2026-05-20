#!/usr/bin/env bash
# scripts/m13.3-lfs-locks-smoke.sh
#
# End-to-end smoke for M13.3 LFS Locks against localfs:
#   1. Build the bucketvcs binary.
#   2. Init a fresh repo + create two users (alice, bob) + tokens + write grants.
#   3. Start the gateway in the background; record PID.
#   4. As alice: POST /locks -> 201 with a new lock_id; record it.
#   5. GET /locks -> 200, includes the new lock.
#   6. POST /locks/verify (as alice) -> ours=[lock], theirs=[].
#   7. POST /locks/verify (as bob) -> ours=[], theirs=[lock].
#   8. POST /locks/<id>/unlock {"force": false} as bob -> 403.
#   9. POST /locks/<id>/unlock {"force": true} as bob -> 200.
#  10. GET /locks -> 200, empty list.
#
# Exits with `M13.3_LFS_LOCKS_SMOKE_OK` on success. Skips with exit 77
# if go / jq / curl is missing.

set -euo pipefail

if ! command -v go >/dev/null 2>&1; then echo "SKIP: go not on PATH"; exit 77; fi
if ! command -v jq >/dev/null 2>&1; then echo "SKIP: jq not on PATH"; exit 77; fi
if ! command -v curl >/dev/null 2>&1; then echo "SKIP: curl not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE="localfs:$ROOT/store"
AUTHDB="$ROOT/auth.db"
TENANT="acme"
REPO="m133smoke"
PORT="$(awk 'BEGIN{srand(); print 30000+int(rand()*10000)}')"
URL="http://127.0.0.1:$PORT"

PID=""
cleanup() {
    rc=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M13.3_LFS_LOCKS_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT; logs at $ROOT/gateway.log)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init + register repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO"
"$BIN" repo register "$TENANT/$REPO" --auth-db="$AUTHDB" --store="$STORE" --no-init

echo "==> Create users + grants + tokens"
"$BIN" user add alice --auth-db="$AUTHDB"
"$BIN" user add bob   --auth-db="$AUTHDB"
"$BIN" repo grant alice "$TENANT/$REPO" write --auth-db="$AUTHDB"
"$BIN" repo grant bob   "$TENANT/$REPO" write --auth-db="$AUTHDB"
ALICE_TOKEN=$("$BIN" token create alice --auth-db="$AUTHDB" | grep -m1 '^bvts_')
BOB_TOKEN=$("$BIN"   token create bob   --auth-db="$AUTHDB" | grep -m1 '^bvts_')
if [[ -z "$ALICE_TOKEN" || -z "$BOB_TOKEN" ]]; then echo "FAIL: could not extract bvts_ tokens"; exit 1; fi
ALICE_AUTH=$(printf "alice:%s" "$ALICE_TOKEN" | base64 -w0)
BOB_AUTH=$(printf "bob:%s" "$BOB_TOKEN" | base64 -w0)

echo "==> Start gateway on $URL"
SIGNING_KEY_FILE="$ROOT/signing.key"
# 32 hex chars = 16 random bytes (>= the 16-byte minimum).
printf '00112233445566778899aabbccddeeff' > "$SIGNING_KEY_FILE"
"$BIN" serve --store="$STORE" --auth-db="$AUTHDB" --addr="127.0.0.1:$PORT" --lfs=true \
    --proxied-url-signing-key="$SIGNING_KEY_FILE" \
    --proxied-url-base="$URL" \
    >"$ROOT/gateway.log" 2>&1 &
PID=$!

# Wait for the gateway to bind.
for i in $(seq 1 50); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$PID" 2>/dev/null; then echo "FAIL: gateway died early"; cat "$ROOT/gateway.log"; exit 1; fi
    sleep 0.2
done

LOCKS_URL="$URL/$TENANT/$REPO.git/info/lfs/locks"

echo "==> Step 4: alice creates a lock"
CREATE=$(curl -sf -X POST -H "Authorization: Basic $ALICE_AUTH" \
    -H "Content-Type: application/vnd.git-lfs+json" \
    --data '{"path":"art/hero.psd"}' \
    "$LOCKS_URL")
LOCK_ID=$(echo "$CREATE" | jq -r '.lock.id')
if [[ -z "$LOCK_ID" || "$LOCK_ID" == "null" ]]; then
    echo "FAIL: create did not return lock.id; body=$CREATE"; exit 1
fi
echo "    lock_id=$LOCK_ID"

echo "==> Step 5: list locks"
LIST=$(curl -sf -H "Authorization: Basic $ALICE_AUTH" "$LOCKS_URL")
COUNT=$(echo "$LIST" | jq '.locks | length')
if [[ "$COUNT" -ne 1 ]]; then echo "FAIL: list count=$COUNT want 1; body=$LIST"; exit 1; fi
echo "    listed=$COUNT lock(s)"

echo "==> Step 6: verify as alice (ours)"
V_ALICE=$(curl -sf -X POST -H "Authorization: Basic $ALICE_AUTH" \
    -H "Content-Type: application/vnd.git-lfs+json" \
    --data '{}' "$LOCKS_URL/verify")
if [[ "$(echo "$V_ALICE" | jq '.ours | length')" -ne 1 ]]; then echo "FAIL: alice ours: $V_ALICE"; exit 1; fi
if [[ "$(echo "$V_ALICE" | jq '.theirs | length')" -ne 0 ]]; then echo "FAIL: alice theirs: $V_ALICE"; exit 1; fi
echo "    alice ours=1 theirs=0"

echo "==> Step 7: verify as bob (theirs)"
V_BOB=$(curl -sf -X POST -H "Authorization: Basic $BOB_AUTH" \
    -H "Content-Type: application/vnd.git-lfs+json" \
    --data '{}' "$LOCKS_URL/verify")
if [[ "$(echo "$V_BOB" | jq '.ours | length')" -ne 0 ]]; then echo "FAIL: bob ours: $V_BOB"; exit 1; fi
if [[ "$(echo "$V_BOB" | jq '.theirs | length')" -ne 1 ]]; then echo "FAIL: bob theirs: $V_BOB"; exit 1; fi
echo "    bob ours=0 theirs=1"

echo "==> Step 8: bob unlock without force -> 403"
HTTP=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Basic $BOB_AUTH" \
    -H "Content-Type: application/vnd.git-lfs+json" --data '{"force":false}' \
    "$LOCKS_URL/$LOCK_ID/unlock")
if [[ "$HTTP" != "403" ]]; then echo "FAIL: bob no-force unlock http=$HTTP want 403"; exit 1; fi
echo "    403 ok"

echo "==> Step 9: bob force unlock -> 200"
HTTP=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Basic $BOB_AUTH" \
    -H "Content-Type: application/vnd.git-lfs+json" --data '{"force":true}' \
    "$LOCKS_URL/$LOCK_ID/unlock")
if [[ "$HTTP" != "200" ]]; then echo "FAIL: bob force unlock http=$HTTP want 200"; exit 1; fi
echo "    200 ok"

echo "==> Step 10: list locks -> empty"
LIST_AFTER=$(curl -sf -H "Authorization: Basic $ALICE_AUTH" "$LOCKS_URL")
COUNT_AFTER=$(echo "$LIST_AFTER" | jq '.locks | length')
if [[ "$COUNT_AFTER" -ne 0 ]]; then echo "FAIL: post-unlock count=$COUNT_AFTER want 0"; exit 1; fi
echo "    empty ok"

echo "M13.3 LFS Locks smoke: OK"
