#!/usr/bin/env bash
# M11 end-to-end smoke against MinIO (S3-compatible direct-mode signed URLs):
#   1. Boot MinIO via docker compose (idempotent).
#   2. Import a tiny git history (import creates the repo in the object store).
#   3. Register the repo in the auth DB and mark it public.
#   4. Run maintenance --force (full pipeline: repack + reachability + bundle).
#   5. Start bucketvcs serve with --bundle-uri-mode=direct + --pack-uri-mode=direct.
#   6. git clone with transfer.bundleURI=true — assert trace contains
#      `X-Amz-Signature` (proves a direct MinIO presigned URL was advertised
#      and the client fetched the bundle directly from object storage).
#   7. git clone with protocol.version=2 + fetch.uriProtocols=https —
#      assert trace contains `X-Amz-Signature` (proves a direct MinIO presigned
#      pack URL was advertised and the client fetched the pack from object
#      storage directly, bypassing the gateway for bulk data).
#   8. Tear down (preserves MinIO container if BUCKETVCS_KEEP_EMULATORS=1).
#
# Note on Test 2 (pack-uri):
#   The client sends `packfile-uris=https` in its fetch request (triggered by
#   -c fetch.uriProtocols=https).  The server's scheme filter keeps "https" in
#   PackfileURIs, so the pack-uri gate fires.  The server mints a presigned
#   MinIO URL via Store.SignedGetURL — MinIO is running over http://, so the
#   URL is http://, not https://.  Git accepts the http:// pack URL regardless
#   of what scheme it advertised to the server.  The X-Amz-Signature in the
#   git http-fetch child_start argv is the strong marker that the direct path
#   fired.
#
# Requirements:
#   - docker compose available
#   - git ≥ 2.41 (transfer.bundleURI support)
#   - Go toolchain available (script builds the binary once before running)
#
# Env var overrides:
#   BUCKETVCS_KEEP_EMULATORS=1   Keep MinIO container running after smoke.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.cloud.yml"
KEEP_UP="${BUCKETVCS_KEEP_EMULATORS:-0}"
BUCKET="bucketvcs-smoke"

# Build the binary once so subsequent calls don't each pay compilation cost.
echo "==> Building bucketvcs binary"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"

echo "==> Booting MinIO via docker compose"
docker compose -f "$COMPOSE_FILE" up -d --wait minio

cleanup() {
    rc=$?
    if [[ -n "${SERVE_PID:-}" ]]; then
        kill "$SERVE_PID" 2>/dev/null || true
        wait "$SERVE_PID" 2>/dev/null || true
    fi
    if [[ -n "${ROOT:-}" ]]; then
        if [[ "$rc" -eq 0 ]]; then
            rm -rf "$ROOT"
        else
            echo "(trace logs and tmpdir preserved at $ROOT for forensic inspection)"
        fi
    fi
    rm -f "$BIN"
    if [[ "$KEEP_UP" != "1" ]]; then
        docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
    fi
    exit "$rc"
}
trap cleanup EXIT

# MinIO bucket creation via mc-in-container (idempotent).
echo "==> Creating MinIO bucket $BUCKET"
docker run --rm --network host \
    --entrypoint sh \
    minio/mc:RELEASE.2025-01-17T23-25-50Z \
    -c "
        mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null
        mc mb --ignore-existing local/$BUCKET >/dev/null
    "

# Env for bucketvcs CLI to talk to MinIO.
export BUCKETVCS_S3_ENDPOINT=http://127.0.0.1:9000
export BUCKETVCS_S3_REGION=us-east-1
export BUCKETVCS_S3_FORCE_PATH_STYLE=true
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin

STORE="s3://$BUCKET"
REPO="r"

ROOT="$(mktemp -d)"
# Use a run-unique tenant so consecutive runs are idempotent without purging the bucket.
TENANT="t$(basename "$ROOT" | tr -dc '[:alnum:]' | head -c 8)"
AUTH_DB="$ROOT/auth.db"

echo "==> Build a tiny git history and import it"
WORK="$ROOT/work"
git init --quiet -b main "$WORK"
echo "hi" > "$WORK/f"
git -C "$WORK" config user.email t@t
git -C "$WORK" config user.name t
git -C "$WORK" add f
git -C "$WORK" commit --quiet -m init

"$BIN" import --store="$STORE" "$WORK" "$TENANT" "$REPO"

echo "==> Register repo in auth DB and mark public"
"$BIN" repo register --auth-db="$AUTH_DB" --no-init "$TENANT/$REPO"
"$BIN" repo public --auth-db="$AUTH_DB" "$TENANT/$REPO" on

echo "==> Run full maintenance (repack + reachability + bundle)"
"$BIN" maintenance --store="$STORE" --repo="$TENANT/$REPO" --force

# Pick a free-ish port for serve.
PORT=$(( 30000 + (RANDOM % 20000) ))

echo "==> Start bucketvcs serve on 127.0.0.1:$PORT (direct mode)"
"$BIN" serve \
    --store="$STORE" \
    --auth-db="$AUTH_DB" \
    --addr="127.0.0.1:$PORT" \
    --bundle-uri-mode=direct \
    --pack-uri-mode=direct \
    --mirror-dir="$ROOT/mirror" &
SERVE_PID=$!

# Poll until serve responds, up to 60s.
READY=0
for _ in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:$PORT/$TENANT/$REPO.git/info/refs?service=git-upload-pack" >/dev/null 2>&1; then
        READY=1
        break
    fi
    sleep 1
done
if [[ "$READY" -eq 0 ]]; then
    echo "FAIL: bucketvcs serve did not respond on port $PORT after 60s"
    exit 1
fi

CLONE_BUNDLE="$ROOT/clone-bundle"
CLONE_PACK="$ROOT/clone-pack"

echo "==> Test 1: git clone with transfer.bundleURI=true (expect bundle-uri direct)"
GIT_TRACE2=1 GIT_TRACE_CURL=1 git \
    -c protocol.version=2 \
    -c transfer.bundleURI=true \
    clone --quiet "http://127.0.0.1:$PORT/$TENANT/$REPO.git" "$CLONE_BUNDLE" \
    2> "$ROOT/trace-bundle.log"

if ! grep -q "X-Amz-Signature" "$ROOT/trace-bundle.log"; then
    echo "FAIL: bundle-uri clone trace did not contain X-Amz-Signature"
    echo "----- trace excerpt -----"
    grep -i "bundle\|signature\|amz" "$ROOT/trace-bundle.log" | head -40
    exit 1
fi
if grep -q -- "--keep=fetch-pack" "$ROOT/trace-bundle.log"; then
    echo "FAIL: bundle-uri clone fell through to standard fetch (--keep=fetch-pack seen)"
    exit 1
fi
echo "    bundle-uri direct signed URL confirmed (X-Amz-Signature present)"

# Test 2: pack-uri direct mode.
# The client sends `packfile-uris=https` (from -c fetch.uriProtocols=https)
# which the server's scheme filter keeps (https is allowed).  The server mints
# a MinIO presigned URL via Store.SignedGetURL — the URL is http:// since
# MinIO runs without TLS, but git accepts the http:// pack URL regardless.
# Strong marker: X-Amz-Signature in git http-fetch child_start argv and
# GIT_TRACE_CURL output.
echo "==> Test 2: git clone with fetch.uriProtocols=https (expect packfile-uri direct)"
GIT_TRACE2=1 GIT_TRACE_CURL=1 git \
    -c protocol.version=2 \
    -c fetch.uriProtocols=https \
    clone --quiet "http://127.0.0.1:$PORT/$TENANT/$REPO.git" "$CLONE_PACK" \
    2> "$ROOT/trace-pack.log"

if ! grep -q "X-Amz-Signature" "$ROOT/trace-pack.log"; then
    echo "FAIL: packfile-uri clone trace did not contain X-Amz-Signature"
    echo "----- trace excerpt -----"
    grep -i "packfile\|signature\|amz" "$ROOT/trace-pack.log" | head -40
    exit 1
fi
echo "    packfile-uri direct signed URL confirmed (X-Amz-Signature present)"

# Sanity: cloned heads match the source HEAD.
SRC_HEAD=$(git -C "$WORK" rev-parse HEAD)
B_HEAD=$(git -C "$CLONE_BUNDLE" rev-parse HEAD)
P_HEAD=$(git -C "$CLONE_PACK" rev-parse HEAD)
if [[ "$SRC_HEAD" != "$B_HEAD" ]] || [[ "$SRC_HEAD" != "$P_HEAD" ]]; then
    echo "FAIL: clone HEAD mismatch (src=$SRC_HEAD bundle-clone=$B_HEAD pack-clone=$P_HEAD)"
    exit 1
fi

echo "M11 MinIO smoke: OK"
