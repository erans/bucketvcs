#!/usr/bin/env bash
# scripts/m13-lfs-smoke-azure.sh
#
# End-to-end smoke for M13 LFS against Azurite (Azure Blob emulator with
# direct-mode SAS URLs):
#   1. Boot Azurite via docker compose (idempotent).
#   2. Create the test container via the Azurite dev account.
#   3. Register a fresh repo against azureblob://<container>.
#   4. Start bucketvcs serve with --lfs and the proxied-url-* flags
#      (HTTPS LFS against Azurite goes direct via the SAS path; the
#      proxied path is wired but should NOT fire).
#   5. git push a small repo with an LFS-tracked binary; assert the
#      Batch + Verify audit events fired AND lfs.object.served did NOT
#      fire (negative marker proves direct mode, not gateway proxy).
#   6. git clone fresh + git lfs pull; same negative-marker assertion.
#   7. Compare bytes round-trip.
#   8. Tear down (preserves Azurite if BUCKETVCS_KEEP_EMULATORS=1, and
#      preserves $ROOT on failure for forensics).
#
# This smoke is the canary for M13.2: Azure Put Blob requires the
# `x-ms-blob-type: BlockBlob` request header. Before M13.2,
# storage.ObjectStore.SignedGetURL had no header-return channel, so the
# header was silently dropped and PUTs got HTTP 400. After M13.2 the
# header travels through the lfs.Store layer and the LFS client
# forwards it. If this smoke ever fails with `HTTP 400`/`InvalidHeader`
# in trace-push.log, the header plumbing has regressed.
#
# Skips with exit 77 (autoconf SKIP convention) if git-lfs, docker,
# docker compose v2, or python3 is unavailable.
#
# Requirements: docker compose, git >= 2.41, git-lfs >= 3.0, Go toolchain,
# python3.
#
# Known race: a transient Python bind+close picks a free port; another
# process can grab it before bucketvcs serve binds. Symptom is healthz
# failing; rerun the script.

set -euo pipefail

if ! command -v git-lfs >/dev/null 2>&1; then
    echo "SKIP: git-lfs not on PATH; install from https://git-lfs.com/"
    exit 77
fi
if ! command -v docker >/dev/null 2>&1; then
    echo "SKIP: docker not on PATH"
    exit 77
fi
if ! docker compose version >/dev/null 2>&1; then
    echo "SKIP: docker compose v2 not available"
    exit 77
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "SKIP: python3 not on PATH (used for the free-port picker)"
    exit 77
fi
# --network host below is a Linux-only Docker primitive. On Docker
# Desktop for macOS/Windows the az-cli container would not reach Azurite
# via http://127.0.0.1:10000 the same way. SKIP on non-Linux until a
# portable wiring (compose project network + service name) is implemented.
if [[ "$(uname -s)" != "Linux" ]]; then
    echo "SKIP: smoke requires Linux Docker (uses --network host for the az-cli container)"
    exit 77
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.cloud.yml"
KEEP_UP="${BUCKETVCS_KEEP_EMULATORS:-0}"
CONTAINER="bucketvcs-lfs-smoke"
# Azurite ships with a single well-known dev account. The connection
# string below is documented in MS-Azurite README and is NOT a secret:
# it is the only credential Azurite accepts and is identical across
# all Azurite installations.
AZURITE_CONN="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

echo "==> Building bucketvcs binary"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

echo "==> Booting Azurite via docker compose"
docker compose -f "$COMPOSE_FILE" up -d --wait azurite

SERVE_PID=""
ROOT="$(mktemp -d)"

cleanup() {
    rc=$?
    if [[ -n "$SERVE_PID" ]]; then
        kill "$SERVE_PID" 2>/dev/null || true
        wait "$SERVE_PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M13_LFS_AZURE_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)"
        if [[ -f "$ROOT/serve.log" ]]; then
            echo "==== serve.log tail ===="
            tail -50 "$ROOT/serve.log" || true
        fi
    fi
    rm -f "$BIN"
    if [[ "$rc" -eq 0 && "$KEEP_UP" != "1" ]]; then
        docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
    elif [[ "$rc" -ne 0 ]]; then
        echo "(Azurite container left running for forensics; tear down with: docker compose -f $COMPOSE_FILE down -v)"
    fi
    exit "$rc"
}
trap cleanup EXIT

echo "==> Creating Azurite container $CONTAINER"
docker run --rm --network host \
    mcr.microsoft.com/azure-cli:2.66.0 \
    az storage container create \
        --name "$CONTAINER" \
        --connection-string "$AZURITE_CONN" \
        >/dev/null

export BUCKETVCS_AZURE_CONTAINER="$CONTAINER"
export BUCKETVCS_AZURE_CONNECTION_STRING="$AZURITE_CONN"

STORE="azureblob://$CONTAINER"
TENANT="t$(basename "$ROOT" | tr -dc '[:alnum:]' | head -c 8)"
REPO="r"

export BUCKETVCS_AUTH_DB="$ROOT/auth.db"
openssl rand -hex 16 > "$ROOT/signing.key"

"$BIN" user add alice
# `token create` prints the token on line 1 and a notice on line 2;
# extract by its bvts_ prefix so a future CLI banner change does not
# silently break. Capture both streams so a CLI error surfaces on the
# diagnostic path below instead of being masked by the regex check.
TOKEN_OUT="$("$BIN" token create alice 2>&1)"
TOKEN=$(printf '%s\n' "$TOKEN_OUT" | grep -m1 '^bvts_' || true)
if [[ ! "$TOKEN" =~ ^bvts_[A-Za-z0-9_]+$ ]]; then
    echo "FAIL: 'bucketvcs token create' output did not match expected bvts_<alphanum_> shape"
    echo "  raw extracted: ${TOKEN:-<empty>}"
    echo "  full CLI output:"
    printf '%s\n' "$TOKEN_OUT" | sed 's/^/    /'
    exit 1
fi
"$BIN" repo register "$TENANT/$REPO" --store "$STORE"
"$BIN" repo grant alice "$TENANT/$REPO" write

PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
BASE_URL="http://127.0.0.1:$PORT"

echo "==> Start bucketvcs serve on $BASE_URL"
"$BIN" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE" \
    --mirror-dir "$ROOT/mirror" \
    --lfs \
    --proxied-url-signing-key "$ROOT/signing.key" \
    --proxied-url-base "$BASE_URL" \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!

for _ in $(seq 1 50); do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 0.2
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "FAIL: serve never ready"; exit 1; }
echo "READY"

WORK="$ROOT/repo"
mkdir -p "$WORK"
(
    cd "$WORK"
    git init -q -b main
    git lfs install --local
    git lfs track "*.bin"
    git config user.email smoke@example.com
    git config user.name smoke
    head -c 1048576 /dev/urandom > big.bin
    git add .gitattributes big.bin
    git commit -qm "add lfs object"
    git remote add origin "http://alice:$TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"
    GIT_TRACE=1 GIT_CURL_VERBOSE=1 git push origin main >"$ROOT/trace-push.log" 2>&1
)

# Strong marker 1: the Batch + Verify audit events fired.
grep -q 'event=lfs.batch'  "$ROOT/serve.log" || { echo "FAIL: missing lfs.batch"; exit 1; }
grep -q 'event=lfs.verify' "$ROOT/serve.log" || { echo "FAIL: missing lfs.verify"; exit 1; }
# Strong marker 2 (negative): the proxied /_lfs/ PUT did NOT fire. If
# event=lfs.object.served appears with op=upload, the client took the
# proxied fallback instead of going direct to Azurite via a SAS URL,
# which means direct mode silently degraded — that is the bug this
# smoke exists to catch.
if grep -qE 'event=lfs\.object\.served.*op=upload' "$ROOT/serve.log"; then
    echo "FAIL: proxied PUT fired (event=lfs.object.served op=upload); LFS did not go direct to Azurite"
    grep 'event=lfs.object.served' "$ROOT/serve.log"
    exit 1
fi
# Strong marker 3 (best-effort): the captured trace should contain a
# SAS-shaped query parameter and the x-ms-blob-type header (the M13.2
# fix). git-lfs trace formats vary across versions; treat the absence
# as a warning unless markers 1+2 also failed.
if ! grep -qiE 'x-ms-blob-type|sig=|se=|sp=' "$ROOT/trace-push.log"; then
    echo "WARN: push trace missing SAS / x-ms-blob-type markers (git-lfs trace format may differ); strong markers 1+2 still satisfied"
fi
echo "    PUSH: lfs.batch + lfs.verify fired; lfs.object.served NOT fired (direct path confirmed)"

CLONE="$ROOT/clone"
GIT_LFS_SKIP_SMUDGE=1 git clone "http://alice:$TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git" "$CLONE"
(
    cd "$CLONE"
    git lfs install --local
    GIT_TRACE=1 GIT_CURL_VERBOSE=1 git lfs pull >"$ROOT/trace-pull.log" 2>&1
)

# Negative-marker check for pull: a download served via the proxied
# path would emit event=lfs.object.served op=download in serve.log.
if grep -qE 'event=lfs\.object\.served.*op=download' "$ROOT/serve.log"; then
    echo "FAIL: proxied GET fired (event=lfs.object.served op=download); LFS did not go direct from Azurite"
    grep 'event=lfs.object.served' "$ROOT/serve.log"
    exit 1
fi
if ! grep -qiE 'sig=|se=|sp=' "$ROOT/trace-pull.log"; then
    echo "WARN: pull trace missing SAS markers (git-lfs trace format may differ); negative marker still satisfied"
fi
echo "    PULL: lfs.object.served op=download NOT fired (direct path confirmed)"

cmp "$WORK/big.bin" "$CLONE/big.bin" || { echo "FAIL: cloned bytes differ"; exit 1; }

echo "M13 LFS Azure smoke: OK"
