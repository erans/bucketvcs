#!/usr/bin/env bash
# scripts/authdb-replica-smoke-localfs.sh
#
# End-to-end smoke for embedded authdb replication (M28: Litestream into
# sys/authdb/ of the object store, single-writer CAS lease at
# sys/authdb/lease.json, restore-on-boot). Four phases:
#
#   Phase 1 — durability through total local-disk loss: replicate, destroy
#             the local sqlite authdb + WAL + litestream scratch, restart,
#             and prove the OLD token still authenticates after restore.
#   Phase 2 — point-in-time restore: snapshot a CUTOFF timestamp, mint a
#             token AFTER it, then restore to a copy bounded at CUTOFF and
#             prove the post-cutoff token is absent (fewer tokens).
#   Phase 3 — single-writer mutual exclusion + lease visibility (see the
#             long design note above Phase 3 — the localfs root lock, not
#             the lease, is what fires here, and that is by design).
#   Phase 4 — true multi-process concurrent CLI writes: while serve replicates,
#             a separate process (this shell) mints a user+token on the same
#             authdb file, then a wipe+restart proves the token survived.
#
# Requires `bucketvcs`, `git`, `curl`, `python3` on PATH. `sqlite3` is
# optional: Phase 2's token-count assertion SKIPs (with a clear message)
# when it is missing, still verifying the PITR copy exists and is
# non-empty. Litestream syncs ~1s after each write, so the script sleeps
# ~2-2.5s at each replication checkpoint.
#
# Backend override: SMOKE_STORE_URL (default localfs:$WORK/store) lets the
# same script run against MinIO/S3 later. The direct-filesystem assertions
# on $STORE/objects/sys/authdb/... and the stale-.lock manipulation in
# Phase 3 only run for the localfs default; they are skipped for any other
# backend (the replica-status / restore CLI assertions still run).
#
# Known race: free ports are picked via a transient Python bind+close, then
# handed to `bucketvcs serve`. Another process can grab the port in the gap;
# symptom is a READY-probe timeout. Rerun the script.

set -euo pipefail

for bin in git curl python3; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "SKIP: $bin not on PATH"
        exit 77
    fi
done

HAVE_SQLITE3=0
command -v sqlite3 >/dev/null 2>&1 && HAVE_SQLITE3=1

WORK="$(mktemp -d)"
echo "smoke work dir: $WORK"

# Backend selection. localfs default enables the direct-filesystem probes;
# any other backend runs only the CLI-level (replica-status/restore) checks.
STORE_URL="${SMOKE_STORE_URL:-localfs:$WORK/store}"
IS_LOCALFS=0
STORE_ROOT=""
if [[ "$STORE_URL" == localfs:* ]]; then
    IS_LOCALFS=1
    STORE_ROOT="${STORE_URL#localfs:}"
    mkdir -p "$STORE_ROOT"
fi

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    # Always try to stop a running serve so the temp dir can be removed and
    # no orphan keeps the localfs lock.
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
    echo "AUTHDB_REPLICA_SMOKE_OK"
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

# start_serve PORT AUTHDB MIRROR LOGFILE — launch a replicating gateway and
# return its PID in the global SERVE_PID. --lfs=false avoids the proxied-URL
# flag requirement; --auth-db-replica=auto replicates into sys/authdb/ of
# --store. A short lease TTL is used so that, after a hard crash (kill -9)
# leaves a still-valid lease behind, the next node can take it over via CAS
# within a few seconds instead of waiting out the 60s default.
LEASE_TTL=5s
LEASE_EXPIRY_WAIT=7  # seconds to wait for a post-crash lease to expire (>TTL)
start_serve() {
    local port="$1" authdb="$2" mirror="$3" logfile="$4"
    "$BUCKETVCS" serve \
        --addr "127.0.0.1:$port" \
        --store "$STORE_URL" \
        --auth-db "$authdb" \
        --mirror-dir "$mirror" \
        --lfs=false \
        --auth-db-replica=auto \
        --auth-db-replica-lease-ttl="$LEASE_TTL" \
        > "$logfile" 2>&1 &
    SERVE_PID=$!
}

# stop_serve_graceful — SIGTERM the current serve and wait. The graceful
# path runs Runner.Close, which performs a final litestream sync, closes the
# store (releasing the localfs root lock), and DELETES sys/authdb/lease.json
# (verified manually: lease.json is present while serving and gone after a
# clean stop). Used before any CLI that must re-open the same localfs root.
stop_serve_graceful() {
    [[ -n "$SERVE_PID" ]] || return 0
    kill -TERM "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
    SERVE_PID=""
}

# 0. Build bucketvcs.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$WORK/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

AUTH_DB="$WORK/auth.db"
LTX0_DIR="$STORE_ROOT/objects/sys/authdb/ltx/0"

# ---------------------------------------------------------------------------
# PHASE 1 — durability through total local-disk loss.
# ---------------------------------------------------------------------------

# 1.1 Start serve; mint admin user + token; register a repo and grant access
#     so the token can read it over HTTP.
PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"

"$BUCKETVCS" user add alice --admin --auth-db "$AUTH_DB" >/dev/null
TOKEN=$("$BUCKETVCS" token create alice --auth-db "$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
if [[ -z "$TOKEN" ]]; then
    echo "could not extract token from 'bucketvcs token create' output"
    exit 1
fi

# Register the repo (incl. M1 bucket init) and grant access BEFORE starting
# serve: repo register --store opens the localfs root, which would collide
# with the exclusive root lock that the running gateway holds.
"$BUCKETVCS" repo register acme/replica-test --auth-db "$AUTH_DB" --store "$STORE_URL"
"$BUCKETVCS" repo grant alice acme/replica-test read --auth-db "$AUTH_DB"

start_serve "$PORT" "$AUTH_DB" "$WORK/mirror1" "$WORK/serve1.log"
wait_ready "$BASE_URL"

# Sanity: the token authenticates against the live (pre-crash) server.
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "alice:$TOKEN" \
    "$BASE_URL/acme/replica-test.git/info/refs?service=git-upload-pack")
if [[ "$code" != "200" ]]; then
    echo "pre-crash auth probe expected 200, got $code"
    exit 1
fi

# 1.2 Wait for litestream to replicate, then verify LTX files landed in the
#     bucket (localfs default only — direct filesystem assertion).
sleep 2.5
if [[ $IS_LOCALFS -eq 1 ]]; then
    if [[ -z "$(ls -A "$LTX0_DIR" 2>/dev/null || true)" ]]; then
        echo "expected non-empty LTX dir at $LTX0_DIR after replication"
        exit 1
    fi
    echo "replicated LTX files: $(ls "$LTX0_DIR" | tr '\n' ' ')"
fi

# 1.3 Simulate total local-disk loss: hard-kill serve, then delete the local
#     authdb and every local litestream artifact. The bucket copy is the
#     ONLY surviving source of truth.
kill -9 "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""

rm -f "$AUTH_DB" "$AUTH_DB"-wal "$AUTH_DB"-shm
rm -rf "$AUTH_DB"-litestream
# kill -9 left the localfs root lock behind (it is a pidfile, not a flock —
# see Phase 3 design note); clear it so the restored gateway can re-open the
# same root. Cloud backends have no such file.
if [[ $IS_LOCALFS -eq 1 ]]; then
    rm -f "$STORE_ROOT/.lock"
fi
if [[ -f "$AUTH_DB" ]]; then
    echo "authdb deletion failed — local copy still present"
    exit 1
fi

# kill -9 never ran Runner.Close, so the dead node's lease.json survives in
# the bucket with a still-valid renewed_at. The restarting gateway must wait
# out the (short) lease TTL so its Lease.Acquire can take over via CAS;
# without this it would fail fast with "lease held by another instance".
# This is the single-writer guarantee doing its job — we just give it time.
sleep "$LEASE_EXPIRY_WAIT"

# 1.4 Restart serve with the same flags. With the local authdb missing it
#     must restore-on-boot from the bucket. The OLD token must still work.
PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"
start_serve "$PORT" "$AUTH_DB" "$WORK/mirror2" "$WORK/serve2.log"
wait_ready "$BASE_URL"

if [[ ! -f "$AUTH_DB" ]]; then
    echo "restore-on-boot did not recreate the local authdb at $AUTH_DB"
    exit 1
fi

code=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "alice:$TOKEN" \
    "$BASE_URL/acme/replica-test.git/info/refs?service=git-upload-pack")
if [[ "$code" != "200" ]]; then
    echo "post-restore auth probe expected 200 for the OLD token, got $code"
    echo "==== serve2.log (tail) ===="
    tail -30 "$WORK/serve2.log" || true
    exit 1
fi
echo "PHASE1_DURABILITY_OK"

# ---------------------------------------------------------------------------
# PHASE 2 — point-in-time restore.
# ---------------------------------------------------------------------------
# serve is up from Phase 1.4. Establish a CUTOFF strictly between the
# already-replicated state and a brand-new "after marker" token.

sleep 2
CUTOFF=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 2

# 2.1 Mint a SECOND token — this one lands AFTER the cutoff. Wait for it to
#     replicate, then stop serve gracefully so the localfs root + lease are
#     released and the restore CLI can open the same store.
"$BUCKETVCS" token create alice --auth-db "$AUTH_DB" --label after-cutoff >/dev/null 2>&1
sleep 2.5
stop_serve_graceful

# 2.2 Restore a copy bounded at CUTOFF. The post-cutoff token must be absent.
PITR_DB="$WORK/pitr.db"
"$BUCKETVCS" authdb restore \
    --replica="$STORE_URL" \
    --output="$PITR_DB" \
    --timestamp="$CUTOFF"

if [[ ! -s "$PITR_DB" ]]; then
    echo "PITR restore produced no/empty file at $PITR_DB"
    exit 1
fi

# 2.3 Compare token counts: the live authdb has the after-cutoff token; the
#     PITR copy (bounded at CUTOFF) must have strictly fewer. Needs sqlite3;
#     SKIP just this assertion (loudly) if it is unavailable.
if [[ $HAVE_SQLITE3 -eq 1 ]]; then
    live_count=$(sqlite3 "$AUTH_DB" "select count(*) from tokens;")
    pitr_count=$(sqlite3 "$PITR_DB" "select count(*) from tokens;")
    echo "token counts: live=$live_count pitr=$pitr_count (cutoff=$CUTOFF)"
    if [[ "$pitr_count" -ge "$live_count" ]]; then
        echo "expected PITR token count ($pitr_count) < live ($live_count): the post-cutoff token leaked into the PITR copy"
        exit 1
    fi
else
    echo "SKIP(phase2 count): sqlite3 not on PATH; verified pitr.db exists and is non-empty ($(wc -c < "$PITR_DB") bytes), but cannot compare token counts"
fi
echo "PHASE2_PITR_OK"

# ---------------------------------------------------------------------------
# PHASE 3 — single-writer mutual exclusion + lease visibility.
# ---------------------------------------------------------------------------
# DESIGN NOTE (localfs specifics — investigated in internal/storage/localfs):
#
#   The single-writer guarantee for authdb replication has TWO enforcement
#   layers. On cloud backends (S3/GCS/Azure) the ONLY guard is the CAS lease
#   at sys/authdb/lease.json — a second gateway's Lease.Acquire sees a live
#   holder and fails with ErrLeaseHeld naming it. On localfs there is an
#   ADDITIONAL, stronger guard that fires first: openStore() -> localfs.Open
#   creates an O_CREATE|O_EXCL pidfile at <store>/.lock and returns
#   ErrAlreadyLocked if it already exists. That check happens BEFORE the
#   replica subsystem (and therefore before the lease) is touched.
#
#   Consequence: on localfs you cannot make a second *live* serve reach the
#   lease check — it is rejected at the store lock with "root is already
#   locked by another instance". And because .lock is a pidfile (NOT a flock
#   tied to a file descriptor), it is NOT released when a process dies by
#   kill -9; it persists on disk. So after a hard crash BOTH the stale .lock
#   AND a still-valid lease.json remain.
#
#   This phase therefore tests the two layers separately and honestly:
#     3a) While serve #1 is live, a second serve against the same store
#         exits non-zero quickly. On localfs that rejection is the .lock
#         (the single-writer guard); its stderr mentions "lock"/"lease".
#     3b) After kill -9, the lease persists. We clear the now-stale .lock
#         (what an operator / the next live node does to reclaim a dead
#         node's localfs root) and run `authdb replica-status`, asserting it
#         reports the surviving lease holder. That proves the lease survives
#         a hard crash and is observable — the cloud-backend takeover path's
#         visibility, exercised at the storage layer.

# 3.1 Restart serve (clean state — graceful stop in Phase 2 released both the
#     localfs lock and the lease).
PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"
start_serve "$PORT" "$AUTH_DB" "$WORK/mirror3" "$WORK/serve3.log"
wait_ready "$BASE_URL"
sleep 2  # let the new serve acquire + replicate a fresh lease

# 3a. Second serve against the SAME store (different authdb path + port) must
#     exit non-zero quickly and name the conflict.
PORT2=$(free_port)
set +e
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT2" \
    --store "$STORE_URL" \
    --auth-db "$WORK/auth-second.db" \
    --mirror-dir "$WORK/mirror3b" \
    --lfs=false \
    --auth-db-replica=auto \
    > "$WORK/serve_second.log" 2>&1
second_exit=$?
set -e
if [[ "$second_exit" -eq 0 ]]; then
    echo "second concurrent serve unexpectedly succeeded (exit 0)"
    exit 1
fi
if ! grep -qiE 'lock|lease' "$WORK/serve_second.log"; then
    echo "second serve failed (exit $second_exit) but stderr did not mention lock/lease:"
    cat "$WORK/serve_second.log"
    exit 1
fi
echo "second serve correctly rejected (exit $second_exit): $(grep -iE 'lock|lease' "$WORK/serve_second.log" | head -1)"

# 3b. Lease persistence + visibility across a hard crash. Only meaningful on
#     localfs (we must clear the stale pidfile by hand to free the store for
#     the replica-status CLI). On other backends, stop gracefully would
#     delete the lease, so we skip the persistence assertion there and just
#     confirm replica-status runs.
kill -9 "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""

if [[ $IS_LOCALFS -eq 1 ]]; then
    # lease.json must still be present (kill -9 never ran Release).
    if [[ ! -f "$STORE_ROOT/objects/sys/authdb/lease.json" ]]; then
        echo "expected lease.json to survive kill -9, but it is gone"
        exit 1
    fi
    # Clear the dead node's stale localfs lock so replica-status can open it.
    rm -f "$STORE_ROOT/.lock"

    status_out=$("$BUCKETVCS" authdb replica-status --replica="$STORE_URL" 2>&1)
    echo "$status_out"
    if ! grep -q '"lease"' <<<"$status_out"; then
        echo "replica-status did not report a surviving lease after kill -9"
        exit 1
    fi
    echo "lease survived kill -9 and is visible via replica-status"
else
    # Non-localfs: no stale-lock problem; the gateway crashed so the lease
    # will expire on its own. Just confirm replica-status runs against the
    # store and reports level data.
    "$BUCKETVCS" authdb replica-status --replica="$STORE_URL" >/dev/null
    echo "replica-status ran against $STORE_URL"
fi
echo "PHASE3_LEASE_OK"

# ---------------------------------------------------------------------------
# PHASE 4 — true multi-process concurrent CLI writes while serve runs.
# ---------------------------------------------------------------------------
# Phases above approximate the "CLI writes while serve replicates" topology in
# process (see internal/authreplica/edgecase_test.go TestEdge_Concurrent...).
# This phase exercises it for REAL: while a replicating gateway holds the authdb
# open, the smoke shell — a genuinely separate OS process — runs `user add` +
# `token create` against the SAME sqlite authdb file (SQLite WAL permits the
# concurrent reader/writer; the CLI touches only --auth-db, never --store, so it
# never contends for the localfs root .lock). We then hard-kill serve, wipe the
# local authdb, wait out the lease, restart, and prove the concurrently-written
# token still authenticates after restore-on-boot — i.e. the gateway replicated
# the other process's writes.
#
# Phase 3 ended with serve hard-killed. On localfs the stale .lock was cleared
# in 3b but lease.json survives; on cloud the lease will expire on its own. Wait
# out the lease either way, then start a fresh gateway.
sleep "$LEASE_EXPIRY_WAIT"

PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"
start_serve "$PORT" "$AUTH_DB" "$WORK/mirror4" "$WORK/serve4.log"
wait_ready "$BASE_URL"
sleep 2  # let the gateway acquire its lease and replicate a fresh baseline

# 4.1 From THIS shell (separate process), mint a new user + token on the live
#     authdb file the gateway is replicating, and grant it read on the repo.
"$BUCKETVCS" user add concurrent --auth-db "$AUTH_DB" >/dev/null
CONC_TOKEN=$("$BUCKETVCS" token create concurrent --auth-db "$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
if [[ -z "$CONC_TOKEN" ]]; then
    echo "could not extract token from concurrent 'bucketvcs token create' output"
    exit 1
fi
"$BUCKETVCS" repo grant concurrent acme/replica-test read --auth-db "$AUTH_DB"

# 4.2 Wait for litestream to replicate the concurrent writes, then hard-kill.
sleep 2.5
kill -9 "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""

# 4.3 Wipe the local authdb + litestream scratch (bucket copy is the only
#     surviving source of truth), clear the stale localfs lock, wait out the
#     lease, restart. Mirrors Phase 1.3's recovery sequence.
rm -f "$AUTH_DB" "$AUTH_DB"-wal "$AUTH_DB"-shm
rm -rf "$AUTH_DB"-litestream
if [[ $IS_LOCALFS -eq 1 ]]; then
    rm -f "$STORE_ROOT/.lock"
fi
if [[ -f "$AUTH_DB" ]]; then
    echo "authdb deletion failed in phase 4 — local copy still present"
    exit 1
fi
sleep "$LEASE_EXPIRY_WAIT"

PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"
start_serve "$PORT" "$AUTH_DB" "$WORK/mirror4b" "$WORK/serve4b.log"
wait_ready "$BASE_URL"

if [[ ! -f "$AUTH_DB" ]]; then
    echo "phase 4 restore-on-boot did not recreate the local authdb at $AUTH_DB"
    exit 1
fi

# 4.4 The concurrently-written token must authenticate after restore (HTTP 200,
#     same probe shape as Phase 1).
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "concurrent:$CONC_TOKEN" \
    "$BASE_URL/acme/replica-test.git/info/refs?service=git-upload-pack")
if [[ "$code" != "200" ]]; then
    echo "phase 4 post-restore auth probe expected 200 for the concurrently-written token, got $code"
    echo "==== serve4b.log (tail) ===="
    tail -30 "$WORK/serve4b.log" || true
    exit 1
fi
echo "PHASE4_CONCURRENT_CLI_OK"

# All four phases passed. The cleanup trap prints AUTHDB_REPLICA_SMOKE_OK.
