#!/usr/bin/env bash
# scripts/m19-multitenant-proxied-smoke.sh
#
# M19 end-to-end smoke: a single bucketvcs gateway serves bundle-uri +
# pack-uri in proxied mode for TWO distinct tenants, and the per-URL
# composite-hash HMAC binds (tenant, repo, object-hash) so cross-tenant
# URL tampering is rejected with 403.
#
# Steps:
#   1. Build bucketvcs. Init authdb. Register users + tokens.
#   2. Import a tiny git history into BOTH acme/r1 and other/r1 against
#      a single localfs store.
#   3. Mark both repos public so unauthenticated curl can fetch their
#      advertised URLs.
#   4. Run maintenance --force on each to materialize bundles.
#   5. Start `bucketvcs serve --bundle-uri-mode=proxied
#      --pack-uri-mode=proxied` with a single signing key + base URL.
#   6. For each tenant, run `git clone -c transfer.bundleURI=true` and
#      grep the GIT_TRACE2 output for the proxied bundle URL.
#   7. Assert each tenant's URL embeds /_bundle/<tenant>/r1/.
#   8. Curl each URL un-modified — expect 200 + non-empty body.
#   9. Tamper-test: swap acme→other in acme's URL (preserving acme's
#      HMAC token) and expect 403 (composite HMAC catches the swap).
#  10. Grep the serve log for "tenant":"acme" and "tenant":"other"
#      proxied.url.served audit events (Task 6 audit attribution).
#
# Exits with `M19_MULTITENANT_PROXIED_SMOKE_OK` on success. Skips with
# exit 77 if go/git/curl are missing.

set -euo pipefail

if ! command -v go   >/dev/null 2>&1; then echo "SKIP: go not on PATH";   exit 77; fi
if ! command -v git  >/dev/null 2>&1; then echo "SKIP: git not on PATH";  exit 77; fi
if ! command -v curl >/dev/null 2>&1; then echo "SKIP: curl not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE_DIR="$ROOT/store"
STORE="localfs:$STORE_DIR"
AUTHDB="$ROOT/auth.db"
SIGNKEY_FILE="$ROOT/sign.key"
SERVE_LOG="$ROOT/serve.log"

# 32 random bytes as signing key file (>= 16 byte minimum enforced by serve.go).
head -c 32 /dev/urandom > "$SIGNKEY_FILE"

# Pick a free port: random pick + collision retry. ss-based listing of
# bound ports is best-effort (port may bind between our check and serve
# start), so we cap retries at 20 attempts.
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

PID=""
cleanup() {
    rc=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M19_MULTITENANT_PROXIED_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT; serve.log + clone traces inside)" >&2
        echo "--- last 80 lines of serve.log ---" >&2
        tail -80 "$SERVE_LOG" 2>/dev/null >&2 || true
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

# Build a tiny git history we can import twice.
build_src() {
    local SRC="$1" TAG="$2"
    git init --quiet -b main "$SRC"
    git -C "$SRC" config user.email m19@test
    git -C "$SRC" config user.name m19
    git -C "$SRC" config commit.gpgsign false
    echo "hello from $TAG" > "$SRC/README.md"
    git -C "$SRC" add README.md
    git -C "$SRC" commit --quiet -m "seed $TAG"
}

echo "==> Build src fixtures"
SRC_ACME="$ROOT/src-acme"
SRC_OTHER="$ROOT/src-other"
build_src "$SRC_ACME"  acme
build_src "$SRC_OTHER" other

echo "==> Import acme/r1 and other/r1 into single store"
"$BIN" import --store="$STORE" "$SRC_ACME"  acme  r1
"$BIN" import --store="$STORE" "$SRC_OTHER" other r1

echo "==> Register repos in authdb and mark public"
"$BIN" repo register acme/r1  --auth-db="$AUTHDB" --no-init
"$BIN" repo register other/r1 --auth-db="$AUTHDB" --no-init
"$BIN" repo public acme/r1  on --auth-db="$AUTHDB"
"$BIN" repo public other/r1 on --auth-db="$AUTHDB"

echo "==> Run maintenance --force on each repo (materialize bundles)"
"$BIN" maintenance --store="$STORE" --repo=acme/r1  --force
"$BIN" maintenance --store="$STORE" --repo=other/r1 --force

echo "==> Start gateway on $URL (bundle-uri + pack-uri proxied)"
"$BIN" serve \
    --store="$STORE" \
    --auth-db="$AUTHDB" \
    --addr="127.0.0.1:$PORT" \
    --lfs=false \
    --mirror-dir="$ROOT/mirror" \
    --bundle-uri-mode=proxied \
    --pack-uri-mode=proxied \
    --proxied-url-signing-key="$SIGNKEY_FILE" \
    --proxied-url-base="$URL" \
    >"$SERVE_LOG" 2>&1 &
PID=$!

for i in $(seq 1 60); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "FAIL: gateway died early"
        cat "$SERVE_LOG"
        exit 1
    fi
    sleep 0.2
done
if ! curl -sf "$URL/healthz" >/dev/null 2>&1; then
    echo "FAIL: gateway never came up"
    exit 1
fi
echo "    serve up on $URL"

# extract_bundle_url runs `git clone -c transfer.bundleURI=true` against the
# gateway with GIT_TRACE_PACKET on, and greps the trace for the proxied
# bundle URL the gateway advertised. Clone output is discarded; we only
# care about the URL appearing in the trace.
extract_bundle_url() {
    local TENANT="$1" REPO="$2"
    local TRACE="$ROOT/trace-$TENANT.log"
    local CLONE_DIR="$ROOT/clone-$TENANT"
    GIT_TRACE2=1 GIT_TRACE_PACKET=1 GIT_TRACE_CURL=1 \
        git -c protocol.version=2 -c transfer.bundleURI=true \
        clone --quiet "$URL/$TENANT/$REPO.git" "$CLONE_DIR" \
        2>"$TRACE" || true
    grep -oE "$URL/_bundle/[A-Za-z0-9._/-]+\?token=[A-Za-z0-9_.=%-]+" "$TRACE" \
        | head -1
}

echo "==> Extract proxied bundle URLs via clone trace"
ACME_URL="$(extract_bundle_url acme  r1)"
OTHER_URL="$(extract_bundle_url other r1)"

[[ -n "$ACME_URL"  ]] || { echo "FAIL: no acme bundle URL extracted from trace";  exit 1; }
[[ -n "$OTHER_URL" ]] || { echo "FAIL: no other bundle URL extracted from trace"; exit 1; }
echo "    ACME_URL=$ACME_URL"
echo "    OTHER_URL=$OTHER_URL"

# Assertion 1: each URL embeds its own tenant + repo segment.
[[ "$ACME_URL" == *"/_bundle/acme/r1/"* ]] \
    || { echo "FAIL: acme URL missing /_bundle/acme/r1/: $ACME_URL"; exit 1; }
[[ "$OTHER_URL" == *"/_bundle/other/r1/"* ]] \
    || { echo "FAIL: other URL missing /_bundle/other/r1/: $OTHER_URL"; exit 1; }
echo "OK   each URL embeds its own /_bundle/<tenant>/r1/ segment"

# Assertion 2: each URL actually serves a non-empty body.
echo "==> Fetch each bundle URL unmodified"
curl -fsS "$ACME_URL"  -o "$ROOT/acme-bundle.bin"
curl -fsS "$OTHER_URL" -o "$ROOT/other-bundle.bin"
[[ -s "$ROOT/acme-bundle.bin"  ]] || { echo "FAIL: acme bundle body empty"; exit 1; }
[[ -s "$ROOT/other-bundle.bin" ]] || { echo "FAIL: other bundle body empty"; exit 1; }
echo "OK   both proxied bundle URLs returned non-empty bodies"

# Assertion 3: cross-tenant URL tamper rejected with 403.
# Swap "acme/r1" → "other/r1" in acme's URL (keeping acme's HMAC token).
# The verify side recomputes the composite "other/r1/<hash>" but the token
# was minted for "acme/r1/<hash>" — HMAC mismatch → 403.
echo "==> Tamper test: swap acme→other in URL, keep acme token"
SWAPPED="${ACME_URL/\/_bundle\/acme\/r1\//\/_bundle\/other\/r1\/}"
[[ "$SWAPPED" != "$ACME_URL" ]] || { echo "FAIL: tamper substitution did not change the URL"; exit 1; }
STATUS="$(curl -sS -o /dev/null -w '%{http_code}' "$SWAPPED")"
if [[ "$STATUS" != "403" ]]; then
    echo "FAIL: cross-tenant URL tamper returned HTTP $STATUS, expected 403"
    echo "      SWAPPED=$SWAPPED"
    exit 1
fi
echo "OK   cross-tenant URL tamper rejected: HTTP 403"

# Assertion 4: audit log carries both tenants (Task 6 attribution).
echo "==> Verify audit log carries proxied.url.served events for both tenants"
# slog default text format: `event=proxied.url.served ... tenant=acme repo=r1`.
# We assert both an event=proxied.url.served line AND tenant=<expected> on
# the same line. Default-formatter quotes only values containing whitespace
# or special characters, so tenant=acme (plain ASCII) is unquoted.
if ! grep -E 'event=proxied\.url\.served.*tenant=acme( |$)' "$SERVE_LOG" >/dev/null; then
    echo "FAIL: serve log missing proxied.url.served tenant=acme audit"
    grep -E 'proxied' "$SERVE_LOG" | head -10 || true
    exit 1
fi
if ! grep -E 'event=proxied\.url\.served.*tenant=other( |$)' "$SERVE_LOG" >/dev/null; then
    echo "FAIL: serve log missing proxied.url.served tenant=other audit"
    grep -E 'proxied' "$SERVE_LOG" | head -10 || true
    exit 1
fi
echo "OK   audit log carries proxied.url.served for both tenants"

echo
echo "All M19 multi-tenant proxied assertions passed."
