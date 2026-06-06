#!/usr/bin/env bash
# scripts/logship-smoke-localfs.sh
#
# End-to-end smoke for usage & activity log shipping (shiplog): serve ships
# two NDJSON streams, gzipped, into sys/logs/<stream>/YYYY/MM/DD/ of the
# object store — activity (the audit taxonomy, slog records tagged
# audit=true) and usage (operation metering: fetch/push/lfs/bundle/pack
# serves). Shipping is ON BY DEFAULT; --log-shipping=off disables it.
#
# Three phases:
#
#   Phase 1 — defaults + fast rotation. With aggressive rotation
#             (--log-ship-interval=5s --log-ship-max-events=5) a real git
#             push and clone over HTTP land usage records, and a
#             bundle-uri-proxied clone lands a shipped AUDIT event
#             (proxied.url.served / bundle.uri.advertised — two of the few
#             events actually tagged audit=true today; see the activity-stream
#             section of docs/operator-guides/log-shipping.md). On SIGTERM the
#             shutdown flush ships any non-empty spool. Assert: the activity
#             stream contains an audit event AND the usage stream contains a
#             "kind":"push" and a "kind":"fetch" record.
#
#             Bundle generation runs `bucketvcs maintenance`, which needs the
#             localfs root lock — so serve must be stopped first. Phase 1
#             therefore runs two SEQUENTIAL serves sharing one spool dir
#             (serve #1 push+clone, then maintenance, then serve #2 bundle-uri
#             clone). Sequential is the key word: serve #1 is fully stopped
#             (graceful SIGTERM drains its spool) before serve #2 boots, so the
#             "one spool dir per LIVE instance" rule is never violated.
#
#   Phase 2 — crash leftovers. Start serve with a long interval
#             (--log-ship-interval=10m) so nothing ships in-band, drive a push
#             to spool some events, then kill -9 (no shutdown flush). A
#             leftover active spool file survives on disk. Restart serve
#             against the same spool dir; boot-time leftover adoption ships it.
#             A graceful stop confirms the pre-crash events reached the bucket.
#
#   Phase 3 — idle no-op. An idle serve (no git operations) with a 1s interval
#             ships NOTHING new: an empty active spool file never rotates and
#             never ships. Assert the sys/logs object count is unchanged.
#
# Requires `bucketvcs`, `git`, `curl`, `python3`, `openssl` on PATH; SKIP
# (exit 77) otherwise. localfs only: direct-filesystem assertions read
# $STORE/objects/sys/logs/. --auth-db-replica defaults OFF, so there is no
# lease to wait out; but a kill -9 still leaves the localfs root .lock pidfile
# behind, so Phase 2 clears the stale .lock before restarting (same recovery
# step the authdb-replica smoke performs).
#
# Markers: PHASE1_SHIP_OK / PHASE2_LEFTOVER_OK / PHASE3_IDLE_OK and finally
# LOGSHIP_SMOKE_OK.
#
# Known race: free ports are picked via a transient Python bind+close, then
# handed to `bucketvcs serve`. Another process can grab the port in the gap;
# symptom is a READY-probe timeout. Rerun the script.

set -euo pipefail

for bin in git curl python3 openssl; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "SKIP: $bin not on PATH"
        exit 77
    fi
done

WORK="$(mktemp -d)"
echo "smoke work dir: $WORK"

STORE_ROOT="$WORK/store"
STORE_URL="localfs:$STORE_ROOT"
mkdir -p "$STORE_ROOT"
AUTH_DB="$WORK/auth.db"
SPOOL_DIR="$WORK/spool"
SIGNING_KEY_FILE="$WORK/signing.key"
LOGS_PREFIX="$STORE_ROOT/objects/sys/logs"

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    # Always stop a running serve so the temp dir can be removed and no orphan
    # keeps the localfs lock.
    if [[ -n "$SERVE_PID" ]]; then
        kill -9 "$SERVE_PID" 2>/dev/null || true
        wait "$SERVE_PID" 2>/dev/null || true
    fi
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== SMOKE FAILED (status=$cleanup_status) ===="
        echo "Preserved work dir for forensics: $WORK"
        for log in "$WORK"/*.log; do
            [[ -f "$log" ]] || continue
            echo "==== $(basename "$log") (last 40 lines) ===="
            tail -40 "$log" || true
        done
        return
    fi
    rm -rf "$WORK"
    echo "LOGSHIP_SMOKE_OK"
}
trap cleanup EXIT

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# wait_ready BASE_URL — poll /healthz for up to ~5s.
wait_ready() {
    local base="$1" i
    for i in $(seq 1 50); do
        if curl -sf "$base/healthz" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.1
    done
    echo "server did not become ready at $base" >&2
    return 1
}

# stop_serve_graceful — SIGTERM the current serve and wait. The graceful path
# runs the shipper's shutdown flush (rotate non-empty actives + ship all
# pending) BEFORE the store closes, then releases the localfs root lock. Used
# before any CLI (e.g. maintenance) that must re-open the same localfs root.
stop_serve_graceful() {
    [[ -n "$SERVE_PID" ]] || return 0
    kill -TERM "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
    SERVE_PID=""
}

# Count shipped gz objects under one stream (or all). Empty when none.
count_logs() {
    local sub="${1:-}"
    find "$LOGS_PREFIX/$sub" -name '*.ndjson.gz' 2>/dev/null | wc -l | tr -d ' '
}

# Concatenate the decompressed contents of one shipped stream to stdout.
dump_stream() {
    local stream="$1" f
    for f in $(find "$LOGS_PREFIX/$stream" -name '*.ndjson.gz' 2>/dev/null); do
        gunzip -c "$f"
    done
}

# 0. Build bucketvcs.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$WORK/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

openssl rand -hex 16 > "$SIGNING_KEY_FILE"

# Common auth setup: admin user, token, repo, write grant. Done with serve
# DOWN so the CLI's --store access does not collide with the root lock.
"$BUCKETVCS" user add alice --admin --auth-db "$AUTH_DB" >/dev/null
TOKEN=$("$BUCKETVCS" token create alice --auth-db "$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
if [[ -z "$TOKEN" ]]; then
    echo "could not extract token from 'bucketvcs token create' output"
    exit 1
fi
"$BUCKETVCS" repo register acme/app --auth-db "$AUTH_DB" --store "$STORE_URL" >/dev/null
"$BUCKETVCS" repo grant alice acme/app write --auth-db "$AUTH_DB"

AUTHED_BASE() { echo "http://alice:$TOKEN@127.0.0.1:$1/acme/app.git"; }

# ---------------------------------------------------------------------------
# PHASE 1 — defaults + fast rotation: real push + clone + bundle-uri clone.
# ---------------------------------------------------------------------------

# 1.1 serve #1 (shipping on, fast rotation). Real push + plain clone produce
#     usage push/fetch records.
PORT=$(free_port)
BASE="http://127.0.0.1:$PORT"
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$WORK/mirror1a" \
    --lfs=false \
    --log-ship-interval=5s \
    --log-ship-max-events=5 \
    --log-spool-dir="$SPOOL_DIR" \
    > "$WORK/serve1a.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE"

SRC="$WORK/src"
rm -rf "$SRC"
git init -q "$SRC"
for i in 1 2 3; do
    git -C "$SRC" -c user.email=a@b.c -c user.name=a commit -q --allow-empty -m "c$i"
done
git -C "$SRC" -c credential.helper= push -q "$(AUTHED_BASE "$PORT")" HEAD:refs/heads/main
git -c credential.helper= clone -q "$(AUTHED_BASE "$PORT")" "$WORK/clone1a"

# Graceful stop drains serve #1's spool to the bucket and releases the lock.
stop_serve_graceful
if [[ -n "$(ls -A "$SPOOL_DIR" 2>/dev/null || true)" ]]; then
    echo "spool dir not drained after graceful stop: $(ls "$SPOOL_DIR")"
    exit 1
fi

# 1.2 Generate a bundle (serve down → root lock free) so a bundle-uri clone
#     advertises + serves one through the gateway, landing an AUDIT event.
"$BUCKETVCS" maintenance --store "$STORE_URL" --repo acme/app --force >/dev/null 2>&1

# 1.3 serve #2 (shipping on, bundle-uri proxied). A bundle-uri clone produces
#     the shipped audit events (bundle.uri.advertised + proxied.url.served).
PORT=$(free_port)
BASE="http://127.0.0.1:$PORT"
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$WORK/mirror1b" \
    --lfs=false \
    --bundle-uri-mode=proxied \
    --proxied-url-signing-key "$SIGNING_KEY_FILE" \
    --proxied-url-base "$BASE" \
    --log-ship-interval=5s \
    --log-ship-max-events=5 \
    --log-spool-dir="$SPOOL_DIR" \
    > "$WORK/serve1b.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE"

git -c credential.helper= -c transfer.bundleURI=true \
    clone -q "$(AUTHED_BASE "$PORT")" "$WORK/clone1b"

# Shutdown flush ships serve #2's spool.
stop_serve_graceful

# 1.4 Assertions: an audit event in activity; push + fetch in usage.
ACTIVITY="$(dump_stream activity)"
USAGE="$(dump_stream usage)"
if ! grep -q '"event":"' <<<"$ACTIVITY"; then
    echo "phase 1: activity stream has no audit event"
    echo "--- activity ---"; echo "$ACTIVITY"
    exit 1
fi
if ! grep -q '"kind":"push"' <<<"$USAGE"; then
    echo "phase 1: usage stream missing a push record"
    echo "--- usage ---"; echo "$USAGE"
    exit 1
fi
if ! grep -q '"kind":"fetch"' <<<"$USAGE"; then
    echo "phase 1: usage stream missing a fetch record"
    echo "--- usage ---"; echo "$USAGE"
    exit 1
fi
echo "phase 1 activity events: $(grep -oE '"event":"[^"]*"' <<<"$ACTIVITY" | sort | uniq -c | tr '\n' ' ')"
echo "phase 1 usage kinds: $(grep -oE '"kind":"[^"]*"' <<<"$USAGE" | sort | uniq -c | tr '\n' ' ')"
echo "PHASE1_SHIP_OK"

# Record the post-phase-1 object counts so phase 3 can prove idle ships nothing.
ACT_COUNT_BEFORE_IDLE=""  # set after phase 2

# ---------------------------------------------------------------------------
# PHASE 2 — crash leftovers shipped on next boot.
# ---------------------------------------------------------------------------
# Use a fresh spool dir: phase 1 drained the shared one, but a dedicated dir
# makes the leftover assertion unambiguous. A long interval (10m) guarantees
# nothing ships in-band, so the events live only in an on-disk spool file when
# we kill -9.
SPOOL2="$WORK/spool2"

PORT=$(free_port)
BASE="http://127.0.0.1:$PORT"
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$WORK/mirror2" \
    --lfs=false \
    --log-ship-interval=10m \
    --log-ship-max-events=100000 \
    --log-spool-dir="$SPOOL2" \
    > "$WORK/serve2.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE"

# Drive a push so the usage stream gets at least one spooled-but-unshipped
# event (interval/threshold are far from triggering).
git -C "$SRC" -c user.email=a@b.c -c user.name=a commit -q --allow-empty -m "phase2"
git -C "$SRC" -c credential.helper= push -q "$(AUTHED_BASE "$PORT")" HEAD:refs/heads/main

# A spool file must exist on disk (nothing shipped yet).
if [[ -z "$(ls -A "$SPOOL2" 2>/dev/null || true)" ]]; then
    echo "phase 2: expected a spooled file before crash, found none"
    exit 1
fi
USAGE_BEFORE_CRASH=$(count_logs usage)

# kill -9: no shutdown flush. The spool file survives.
kill -9 "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""
# kill -9 leaves the localfs root .lock pidfile behind (it is a pidfile, not a
# flock — see the authdb-replica smoke's Phase 3 note); clear it so the
# restarting gateway can re-open the same root.
rm -f "$STORE_ROOT/.lock"

if [[ -z "$(ls -A "$SPOOL2" 2>/dev/null || true)" ]]; then
    echo "phase 2: spool file did not survive kill -9"
    exit 1
fi

# Restart against the SAME spool dir; boot-time leftover adoption ships it.
PORT=$(free_port)
BASE="http://127.0.0.1:$PORT"
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$WORK/mirror2b" \
    --lfs=false \
    --log-ship-interval=5s \
    --log-ship-max-events=5 \
    --log-spool-dir="$SPOOL2" \
    > "$WORK/serve2b.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE"
# Give the boot-leftover ship loop a tick or two to flush.
sleep 2
stop_serve_graceful

USAGE_AFTER_RESTART=$(count_logs usage)
if [[ "$USAGE_AFTER_RESTART" -le "$USAGE_BEFORE_CRASH" ]]; then
    echo "phase 2: pre-crash spooled events were not shipped on next boot (usage objects $USAGE_BEFORE_CRASH -> $USAGE_AFTER_RESTART)"
    exit 1
fi
# The spool dir must be empty once leftovers shipped.
if [[ -n "$(ls -A "$SPOOL2" 2>/dev/null || true)" ]]; then
    echo "phase 2: spool not empty after leftover ship: $(ls "$SPOOL2")"
    exit 1
fi
echo "phase 2 usage objects: before-crash=$USAGE_BEFORE_CRASH after-restart=$USAGE_AFTER_RESTART"
echo "PHASE2_LEFTOVER_OK"

# ---------------------------------------------------------------------------
# PHASE 3 — idle serve ships nothing.
# ---------------------------------------------------------------------------
ALL_BEFORE_IDLE=$(count_logs)
SPOOL3="$WORK/spool3"

PORT=$(free_port)
BASE="http://127.0.0.1:$PORT"
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$WORK/mirror3" \
    --lfs=false \
    --log-ship-interval=1s \
    --log-ship-max-events=5 \
    --log-spool-dir="$SPOOL3" \
    > "$WORK/serve3.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE"
# Stay idle: no git operations. With a 1s interval an empty active spool file
# must never rotate and never ship.
sleep 3
stop_serve_graceful

ALL_AFTER_IDLE=$(count_logs)
if [[ "$ALL_AFTER_IDLE" -ne "$ALL_BEFORE_IDLE" ]]; then
    echo "phase 3: idle serve shipped new objects ($ALL_BEFORE_IDLE -> $ALL_AFTER_IDLE)"
    exit 1
fi
echo "phase 3 sys/logs objects unchanged while idle: $ALL_AFTER_IDLE"
echo "PHASE3_IDLE_OK"

# All three phases passed. The cleanup trap prints LOGSHIP_SMOKE_OK.
