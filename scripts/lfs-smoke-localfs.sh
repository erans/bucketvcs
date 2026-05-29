#!/usr/bin/env bash
# scripts/lfs-smoke-localfs.sh
#
# End-to-end smoke test for Git LFS against a localfs gateway. Drives
# the stock `git-lfs` client through push and pull, verifying that the
# batch + transfer + verify endpoints all work together. Needs no cloud
# credentials — the local counterpart to the scripts/e2e-*-push.sh suite.
#
# Skips with exit 77 if git-lfs is not installed (autoconf SKIP
# convention). Requires `bucketvcs` and `git` on PATH.
#
# Known race: we pick a free port via a transient Python bind+close,
# then ask `bucketvcs serve` to bind it. Another process can grab the
# port in the gap between close() and bind(). Symptom is a "READY"
# failure on the healthz probe; rerun the script.

set -euo pipefail

if ! command -v git-lfs >/dev/null 2>&1; then
    echo "SKIP: git-lfs not on PATH; install from https://git-lfs.com/"
    exit 77
fi

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
        if [[ -n "$SERVE_PID" ]]; then
            kill "$SERVE_PID" 2>/dev/null || true
        fi
        return
    fi
    [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$ROOT"
    echo "LFS_LOCALFS_SMOKE_OK"
}
trap cleanup EXIT

# 1. Build bucketvcs.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$ROOT/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

# 2. Set up auth + signing key.
STORE_DIR="$ROOT/store"
mkdir -p "$STORE_DIR"
AUTH_DB="$ROOT/auth.db"
SIGNING_KEY_FILE="$ROOT/signing.key"
openssl rand -hex 16 > "$SIGNING_KEY_FILE"

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
BASE_URL="http://127.0.0.1:$PORT"

"$BUCKETVCS" user add alice --auth-db "$AUTH_DB"
# Extract the token by its bvts_ prefix rather than positional line
# selection, so a future CLI banner or notice rearrangement does not
# silently bind TOKEN to non-secret text.
TOKEN=$("$BUCKETVCS" token create alice --auth-db "$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
if [[ -z "$TOKEN" ]]; then
    echo "could not extract token from 'bucketvcs token create' output"
    exit 1
fi
"$BUCKETVCS" repo register acme/lfs-test --auth-db "$AUTH_DB" --store "localfs:$STORE_DIR"
"$BUCKETVCS" repo grant alice acme/lfs-test write --auth-db "$AUTH_DB"

# 3. Launch server.
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "localfs:$STORE_DIR" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$ROOT/mirror" \
    --lfs \
    --proxied-url-signing-key "$SIGNING_KEY_FILE" \
    --proxied-url-base "$BASE_URL" \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!

# 4. Wait for the server to be ready.
for i in $(seq 1 50); do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "server did not start"; exit 1; }
echo "READY"

# 5. Create a local repo with an LFS-tracked file.
REPO="$ROOT/repo"
mkdir -p "$REPO"
( cd "$REPO" && \
    git init -q -b main && \
    git lfs install --local && \
    git lfs track "*.bin" && \
    git config user.email smoke@example.com && \
    git config user.name smoke && \
    head -c 1048576 /dev/urandom > big.bin && \
    git add .gitattributes big.bin && \
    git commit -qm "add lfs object" && \
    git remote add origin "http://alice:$TOKEN@127.0.0.1:$PORT/acme/lfs-test.git" && \
    git push origin main )

# 6. Confirm batch+verify markers in the server log.
grep -q 'event=lfs.batch' "$ROOT/serve.log" || { echo "missing lfs.batch event"; exit 1; }
grep -q 'event=lfs.object.served' "$ROOT/serve.log" || { echo "missing lfs.object.served event"; exit 1; }
grep -q 'event=lfs.verify' "$ROOT/serve.log" || { echo "missing lfs.verify event"; exit 1; }
echo "LFS_SMOKE_PUSH_OK"

# 7. Clone fresh and confirm the LFS object pulls.
CLONE="$ROOT/clone"
GIT_LFS_SKIP_SMUDGE=1 git clone "http://alice:$TOKEN@127.0.0.1:$PORT/acme/lfs-test.git" "$CLONE"
( cd "$CLONE" && git lfs install --local && git lfs pull )
test -s "$CLONE/big.bin" || { echo "cloned LFS object is empty"; exit 1; }
cmp "$REPO/big.bin" "$CLONE/big.bin" || { echo "cloned LFS bytes differ"; exit 1; }
echo "LFS_SMOKE_PULL_OK"

# 8. Done.
