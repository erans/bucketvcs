#!/usr/bin/env bash
# scripts/m13-lfs-smoke-minio.sh
#
# End-to-end smoke for M13 LFS against MinIO (S3-compatible direct-mode
# signed URLs):
#   1. Boot MinIO via docker compose (idempotent).
#   2. Create the test bucket.
#   3. Register a fresh repo against s3://bucket (init creates the
#      manifest under the unique tenant prefix).
#   4. Start bucketvcs serve with --lfs and the proxied-url-* flags
#      (the SSH path is not exercised here; HTTPS LFS against MinIO
#      goes direct via the S3 presign path).
#   5. git push a small repo with an LFS-tracked binary; assert the
#      git-lfs client trace shows `X-Amz-Signature` in the upload PUT
#      (proves direct upload, not gateway proxy).
#   6. git clone fresh + git lfs pull; assert the trace shows
#      `X-Amz-Signature` in the download GET. Compare bytes.
#   7. Tear down (preserves MinIO if BUCKETVCS_KEEP_EMULATORS=1, and
#      preserves $ROOT on failure for forensics).
#
# Skips with exit 77 (autoconf SKIP convention) if git-lfs, docker, or
# docker compose v2 is unavailable.
#
# Requirements: docker compose, git >= 2.41, git-lfs >= 3.0, Go toolchain.
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
# Desktop for macOS/Windows the mc container would not reach MinIO via
# http://localhost:9000 the same way. SKIP on non-Linux until a portable
# wiring (compose project network + service name) is implemented.
if [[ "$(uname -s)" != "Linux" ]]; then
    echo "SKIP: smoke requires Linux Docker (uses --network host for the mc container)"
    exit 77
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.cloud.yml"
KEEP_UP="${BUCKETVCS_KEEP_EMULATORS:-0}"
BUCKET="bucketvcs-lfs-smoke"

echo "==> Building bucketvcs binary"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
# mktemp creates with mode 0600; some Go toolchains/linkers do not
# re-chmod the output file, so be explicit.
chmod +x "$BIN"

echo "==> Booting MinIO via docker compose"
docker compose -f "$COMPOSE_FILE" up -d --wait minio

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
        echo "M13_LFS_MINIO_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)"
        if [[ -f "$ROOT/serve.log" ]]; then
            echo "==== serve.log tail ===="
            tail -50 "$ROOT/serve.log" || true
        fi
    fi
    rm -f "$BIN"
    # On failure, keep MinIO up regardless of KEEP_UP so operators can
    # `mc ls` the bucket while investigating.
    if [[ "$rc" -eq 0 && "$KEEP_UP" != "1" ]]; then
        docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
    elif [[ "$rc" -ne 0 ]]; then
        echo "(MinIO container left running for forensics; tear down with: docker compose -f $COMPOSE_FILE down -v)"
    fi
    exit "$rc"
}
trap cleanup EXIT

echo "==> Creating MinIO bucket $BUCKET"
docker run --rm --network host \
    --entrypoint sh \
    minio/mc:RELEASE.2025-01-17T23-25-50Z \
    -c "
        mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null
        mc mb --ignore-existing local/$BUCKET >/dev/null
    "

export BUCKETVCS_S3_ENDPOINT=http://127.0.0.1:9000
export BUCKETVCS_S3_REGION=us-east-1
export BUCKETVCS_S3_FORCE_PATH_STYLE=true
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin

STORE="s3://$BUCKET"
TENANT="t$(basename "$ROOT" | tr -dc '[:alnum:]' | head -c 8)"
REPO="r"

export BUCKETVCS_AUTH_DB="$ROOT/auth.db"
openssl rand -hex 16 > "$ROOT/signing.key"

"$BIN" user add alice
# `token create` prints the token on line 1 and a notice on line 2; extract
# by its bvts_ prefix so a future CLI banner change does not silently break.
# `|| true` keeps pipefail from killing the script before the diagnostic
# below has a chance to fire.
TOKEN=$("$BIN" token create alice 2>/dev/null | sed -n 's/^token=//p' | head -1 || true)
if [[ ! "$TOKEN" =~ ^bvts_[A-Za-z0-9_]+$ ]]; then
    echo "FAIL: 'bucketvcs token create' output did not match expected bvts_<alphanum_> shape"
    echo "  raw extracted: ${TOKEN:-<empty>}"
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
    # GIT_TRACE catches git-lfs trace lines (git-lfs honors GIT_TRACE);
    # GIT_CURL_VERBOSE surfaces the inner HTTP exchanges. Some git-lfs
    # versions emit request headers to stdout, so capture both streams.
    GIT_TRACE=1 GIT_CURL_VERBOSE=1 git push origin main >"$ROOT/trace-push.log" 2>&1
)

# Strong marker 1: the Batch + Verify audit events fired.
grep -q 'event=lfs.batch'  "$ROOT/serve.log" || { echo "FAIL: missing lfs.batch"; exit 1; }
grep -q 'event=lfs.verify' "$ROOT/serve.log" || { echo "FAIL: missing lfs.verify"; exit 1; }
# Strong marker 2 (negative): the proxied /_lfs/ PUT did NOT fire. If
# event=lfs.object.served appears with op=upload, the client took the
# proxied fallback instead of going direct to MinIO via a presigned URL,
# which means direct mode silently degraded — that is the bug this smoke
# exists to catch.
if grep -qE 'event=lfs\.object\.served.*op=upload' "$ROOT/serve.log"; then
    echo "FAIL: proxied PUT fired (event=lfs.object.served op=upload); LFS did not go direct to MinIO"
    grep 'event=lfs.object.served' "$ROOT/serve.log"
    exit 1
fi
# Strong marker 3 (best-effort): the captured trace should contain an
# X-Amz-* header somewhere (signature, date, or content-sha256). git-lfs
# trace formats vary across versions; treat the absence as a warning
# unless markers 1+2 also failed.
if ! grep -qiE 'x-amz-(signature|date|content-sha256)' "$ROOT/trace-push.log"; then
    echo "WARN: push trace missing X-Amz-* markers (git-lfs trace format may differ); strong markers 1+2 still satisfied"
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
    echo "FAIL: proxied GET fired (event=lfs.object.served op=download); LFS did not go direct from MinIO"
    grep 'event=lfs.object.served' "$ROOT/serve.log"
    exit 1
fi
if ! grep -qiE 'x-amz-(signature|date|content-sha256)' "$ROOT/trace-pull.log"; then
    echo "WARN: pull trace missing X-Amz-* markers (git-lfs trace format may differ); negative marker still satisfied"
fi
echo "    PULL: lfs.object.served op=download NOT fired (direct path confirmed)"

cmp "$WORK/big.bin" "$CLONE/big.bin" || { echo "FAIL: cloned bytes differ"; exit 1; }

echo "M13 LFS MinIO smoke: OK"
