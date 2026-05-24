#!/usr/bin/env bash
# scripts/m18-rate-limit-smoke.sh
#
# End-to-end smoke for M18 HTTPS auth rate-limiting against localfs:
#   1. Build bucketvcs; init a repo + authdb.
#   2. Create user 'alice' with read grant. Issue a repo:read token.
#   3. Start `bucketvcs serve` with --auth-rate-limit-burst=3
#      --auth-rate-limit-refill-per-minute=60 (low burst + fast refill so the
#      smoke runs in seconds, not minutes).
#   4. Hammer up to 6 fast probes with WRONG password — expect the first
#      requests to return 401 (bad creds), then once the bucket overflows,
#      subsequent probes return 429 + Retry-After.
#      (Note: with refill=60/min=1/sec, decay between back-to-back probes
#      is non-zero — the limiter trips between probes 4 and 5 depending on
#      elapsed wall time. We assert at least one 401 AND at least one 429.)
#   5. Sleep 3s (60-per-minute decay ⇒ ~3 failures cleared from the bucket).
#   6. Probe with WRONG password — expect 401 (the limiter has decayed back
#      below burst, NOT 429).
#   7. Probe with CORRECT password — expect 200 (limiter doesn't block good
#      creds; MarkSuccess fully resets the bucket).
#   8. Assert `auth.ratelimit.hit` audit appears in the serve log.
#
# Exits with `M18_RATE_LIMIT_SMOKE_OK` on success. Skips with exit 77 if
# go or curl is missing.

set -euo pipefail

if ! command -v go   >/dev/null 2>&1; then echo "SKIP: go not on PATH";   exit 77; fi
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
REPO="m18smoke"
PORT="$(awk 'BEGIN{srand(); print 30000+int(rand()*10000)}')"
URL="http://127.0.0.1:$PORT"
SERVE_LOG="$ROOT/gateway.log"

PID=""
cleanup() {
    rc=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M18_RATE_LIMIT_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT; gateway.log + probe-*.out files)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init + register repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO"
"$BIN" repo register "$TENANT/$REPO" --auth-db="$AUTHDB" --store="$STORE" --no-init

echo "==> Create user + grant read"
"$BIN" user add alice --auth-db="$AUTHDB"
"$BIN" repo grant alice "$TENANT/$REPO" read --auth-db="$AUTHDB"

echo "==> Issue repo:read token"
"$BIN" token create alice --auth-db="$AUTHDB" --scopes=repo:read \
    >"$ROOT/token.out" 2>"$ROOT/token.err"
TOKEN=$(grep -m1 '^token=' "$ROOT/token.out" | sed 's/^token=//')
if [[ -z "$TOKEN" ]]; then
    echo "FAIL: token output missing token="
    echo "--- stdout ---"; cat "$ROOT/token.out"
    echo "--- stderr ---"; cat "$ROOT/token.err"
    exit 1
fi
echo "    token issued"

echo "==> Start gateway on $URL (burst=3, refill=60/min)"
"$BIN" serve \
    --store="$STORE" \
    --auth-db="$AUTHDB" \
    --addr="127.0.0.1:$PORT" \
    --lfs=false \
    --mirror-dir="$ROOT/mirror" \
    --auth-rate-limit-burst=3 \
    --auth-rate-limit-refill-per-minute=60 \
    >"$SERVE_LOG" 2>&1 &
PID=$!
for i in $(seq 1 50); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "FAIL: gateway died early"
        cat "$SERVE_LOG"
        exit 1
    fi
    sleep 0.2
done

PROBE_PATH="/$TENANT/$REPO.git/info/refs?service=git-upload-pack"

# probe: print HTTP status + (if present) Retry-After header value.
# Emits two lines: STATUS=<code> and (optional) RETRY_AFTER=<seconds>.
probe() {
    local user="$1" pass="$2" hdrs="$3"
    curl -sS -D "$hdrs" -o /dev/null -w 'STATUS=%{http_code}\n' \
        -u "${user}:${pass}" \
        "${URL}${PROBE_PATH}"
}

retry_after_from() {
    local hdrs="$1"
    grep -i '^Retry-After:' "$hdrs" | awk '{print $2}' | tr -d '\r' | head -n1
}

echo "==> Hammer up to 6 fast probes with WRONG password"
declare -a STATUSES=()
SAW_401=0
SAW_429=0
FIRST_429_HDRS=""
for i in 1 2 3 4 5 6; do
    HDRS="$ROOT/probe${i}.hdrs"
    OUT=$(probe alice "wrongpass" "$HDRS")
    ST=$(grep '^STATUS=' <<<"$OUT" | cut -d= -f2 | tr -d '\r\n')
    STATUSES+=("$ST")
    echo "    probe $i: HTTP $ST"
    case "$ST" in
        401) SAW_401=1 ;;
        429)
            SAW_429=1
            if [[ -z "$FIRST_429_HDRS" ]]; then
                FIRST_429_HDRS="$HDRS"
            fi
            ;;
        *)
            echo "FAIL: probe $i returned unexpected HTTP $ST (expected 401 or 429)"
            echo "--- gateway log tail ---"
            tail -40 "$SERVE_LOG" >&2 || true
            exit 1
            ;;
    esac
done

if [[ "$SAW_401" -ne 1 ]]; then
    echo "FAIL: never saw a 401 — bad creds should yield 401 before bucket fills"
    echo "    statuses: ${STATUSES[*]}"
    exit 1
fi
echo "OK   saw at least one 401 (bad creds before bucket filled)"

if [[ "$SAW_429" -ne 1 ]]; then
    echo "FAIL: never saw a 429 — bucket should overflow within 6 probes"
    echo "    statuses: ${STATUSES[*]}"
    echo "--- gateway log tail ---"
    tail -40 "$SERVE_LOG" >&2 || true
    exit 1
fi
echo "OK   saw at least one 429 (rate-limited)"

# Retry-After must be a positive integer on the first 429.
RETRY_AFTER=$(retry_after_from "$FIRST_429_HDRS")
if [[ -z "$RETRY_AFTER" ]]; then
    echo "FAIL: no Retry-After header on first 429 response"
    echo "--- first 429 hdrs ---"; cat "$FIRST_429_HDRS"
    exit 1
fi
if ! [[ "$RETRY_AFTER" =~ ^[0-9]+$ ]] || [[ "$RETRY_AFTER" -lt 1 ]]; then
    echo "FAIL: Retry-After=$RETRY_AFTER is not a positive integer"
    exit 1
fi
echo "OK   Retry-After: ${RETRY_AFTER}s"

echo "==> Sleep 3s to let the bucket decay below burst (60/min ⇒ ~3 failures cleared)"
sleep 3

echo "==> Probe with WRONG password after decay (expect 401, not 429)"
HDRS_D="$ROOT/probe-decay.hdrs"
OUT_D=$(probe alice "wrongpass" "$HDRS_D")
ST_D=$(grep '^STATUS=' <<<"$OUT_D" | cut -d= -f2 | tr -d '\r\n')
if [[ "$ST_D" != "401" ]]; then
    echo "FAIL: post-decay probe expected 401, got $ST_D"
    echo "--- hdrs ---"; cat "$HDRS_D"
    echo "--- gateway log tail ---"
    tail -40 "$SERVE_LOG" >&2 || true
    exit 1
fi
echo "OK   post-decay probe: HTTP 401 (bucket decayed below burst)"

echo "==> Probe with GOOD password (expect 200 — limiter doesn't block; MarkSuccess resets)"
HDRS_G="$ROOT/probe-good.hdrs"
OUT_G=$(probe alice "$TOKEN" "$HDRS_G")
ST_G=$(grep '^STATUS=' <<<"$OUT_G" | cut -d= -f2 | tr -d '\r\n')
if [[ "$ST_G" != "200" ]]; then
    echo "FAIL: good-creds probe expected 200, got $ST_G"
    echo "--- hdrs ---"; cat "$HDRS_G"
    echo "--- gateway log tail ---"
    tail -40 "$SERVE_LOG" >&2 || true
    exit 1
fi
echo "OK   good-creds probe: HTTP 200"

echo "==> Assert auth.ratelimit.hit audit in serve log"
# slog default flushes synchronously, but give a tick of safety margin.
sleep 0.3
if ! grep -q 'auth.ratelimit.hit' "$SERVE_LOG"; then
    echo "FAIL: auth.ratelimit.hit audit not found in serve log"
    echo "--- serve log tail ---"
    tail -80 "$SERVE_LOG" >&2 || true
    exit 1
fi
HIT_COUNT=$(grep -c 'auth.ratelimit.hit' "$SERVE_LOG" || true)
echo "OK   auth.ratelimit.hit present ($HIT_COUNT occurrence(s))"

echo "==> M18 rate-limit smoke: OK"
