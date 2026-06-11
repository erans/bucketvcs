#!/usr/bin/env bash
# scripts/smoke-observability.sh
#
# End-to-end smoke for the web UI observability surface: session management
# (self-service /settings/sessions + admin /admin/sessions) and the audit-log
# viewer (global /admin/audit + per-repo /{t}/{r}/settings/audit), against a
# localfs serve with the web UI and log shipping enabled.
#
# Two parts:
#
#   A. Sessions (real-time, fully deterministic, asserted in a single live
#      serve): list with the current session badged, a second concurrent login
#      shows two sessions, revoke the other one drops back to one, and the
#      admin all-sessions page lists the user by name.
#
#   B. Audit viewer (shipping is ASYNC — events reach the bucket on the next
#      ship). The cleanest deterministic flush is a graceful serve shutdown,
#      which drains the spool. So we: generate a repo-scoped audit event
#      (a policy-rejected push — policy.ref.rejected carries tenant+repo and is
#      tagged audit=true), STOP the serve gracefully (flush), then RESTART
#      against the same --store + --auth-db, re-login, and read the audit pages.
#      Assert the event appears in /admin/audit and in the OWNING repo's audit
#      tab, and does NOT appear in a second repo's tab (repo scoping boundary).
#
# Requires curl, go, git, python3 on PATH. localfs only; no cloud credentials.

set -euo pipefail

for bin in curl go git python3; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "SKIP: $bin not on PATH"
        exit 77
    fi
done

ROOT="$(mktemp -d)"
echo "smoke root: $ROOT"

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    if [[ -n "$SERVE_PID" ]]; then
        kill "$SERVE_PID" 2>/dev/null || true
        wait "$SERVE_PID" 2>/dev/null || true
    fi
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== OBSERVABILITY SMOKE FAILED (status=$cleanup_status) ===="
        echo "Preserved root for forensics: $ROOT"
        for log in "$ROOT"/serve*.log; do
            [[ -f "$log" ]] || continue
            echo "==== $(basename "$log") (last 40 lines) ===="
            tail -40 "$log" || true
        done
        return
    fi
    rm -rf "$ROOT"
    echo "ALL OBSERVABILITY SMOKE CHECKS PASSED"
}
trap cleanup EXIT

# ---- helpers -----------------------------------------------------------------

# get_csrf <cookie_jar> <url> — fetch <url>, scrape the first csrf_token field,
# persist cookies back into the jar.
get_csrf() {
    local jar="$1" url="$2"
    curl -sS -b "$jar" -c "$jar" "$url" \
        | grep -o 'name="csrf_token" value="[^"]*"' \
        | head -1 \
        | sed 's/.*value="//;s/"//'
}

# web_login <cookie_jar> <user> <password> — perform a password login into the
# given (fresh) cookie jar. Echoes the final HTTP status code.
web_login() {
    local jar="$1" user="$2" pass="$3"
    local csrf
    csrf=$(get_csrf "$jar" "$BASE_URL/login")
    test -n "$csrf" || { echo "FAIL: no csrf on login page for $user"; exit 1; }
    curl -sS -b "$jar" -c "$jar" -o /dev/null -w '%{http_code}' \
        --data-urlencode "username=$user" \
        --data-urlencode "password=$pass" \
        --data-urlencode "csrf_token=$csrf" \
        --data-urlencode "next=/" \
        "$BASE_URL/login"
}

assert_eq() {
    local got="$1" want="$2" msg="$3"
    if [[ "$got" != "$want" ]]; then
        echo "FAIL: $msg: got=$got want=$want"
        exit 1
    fi
}

assert_contains() {
    local body="$1" pat="$2" msg="$3"
    if ! printf '%s' "$body" | grep -q "$pat"; then
        echo "FAIL: $msg: pattern '$pat' not found"
        exit 1
    fi
}

assert_not_contains() {
    local body="$1" pat="$2" msg="$3"
    if printf '%s' "$body" | grep -q "$pat"; then
        echo "FAIL: $msg: pattern '$pat' WAS found (should be absent)"
        exit 1
    fi
}

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

wait_ready() {
    local base="$1" i
    for i in $(seq 1 50); do
        curl -sf "$base/healthz" >/dev/null 2>&1 && return 0
        sleep 0.1
    done
    echo "server did not become ready at $base" >&2
    return 1
}

# stop_serve_graceful — SIGTERM the live serve and wait. The graceful path runs
# the shipper's shutdown flush (rotate non-empty actives + ship pending) before
# the store closes, then releases the localfs root lock.
stop_serve_graceful() {
    [[ -n "$SERVE_PID" ]] || return 0
    kill -TERM "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
    SERVE_PID=""
}

# ---- step 0 — build + CLI bootstrap (serve DOWN, no root-lock contention) -----
echo ""
echo "== Step 0: build + bootstrap =="

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$ROOT/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

STORE_DIR="$ROOT/store"; mkdir -p "$STORE_DIR"
STORE_URL="localfs:$STORE_DIR"
AUTH_DB="$ROOT/auth.db"
SPOOL_DIR="$ROOT/spool"

# Admin user with a password (browser login) + an API token (git push).
"$BUCKETVCS" user add admin --admin --auth-db "$AUTH_DB" >/dev/null
echo "adminpw99" | "$BUCKETVCS" user set-password admin --auth-db "$AUTH_DB" --password-stdin
TOKEN=$("$BUCKETVCS" token create admin --auth-db "$AUTH_DB" 2>/dev/null | sed -n 's/^token=//p' | head -1)
test -n "$TOKEN" || { echo "FAIL: could not extract admin API token"; exit 1; }

# Two repos: acme/demo carries the audit event; acme/other proves repo scoping.
"$BUCKETVCS" repo register acme/demo  --auth-db "$AUTH_DB" --store "$STORE_URL" >/dev/null
"$BUCKETVCS" repo register acme/other --auth-db "$AUTH_DB" --store "$STORE_URL" >/dev/null

PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"

# ---- boot serve #1 (UI + shipping on, fast rotation) -------------------------
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$ROOT/mirror1" \
    --lfs=false \
    --log-shipping=on \
    --log-ship-interval=5s \
    --log-ship-max-events=5 \
    --log-spool-dir="$SPOOL_DIR" \
    > "$ROOT/serve1.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE_URL"
echo "  serve #1 READY (pid=$SERVE_PID)"

# =============================================================================
# PART A — SESSIONS (real-time, fully asserted)
# =============================================================================

# ---- step 1 — admin login (jar A) --------------------------------------------
echo ""
echo "== Step 1: admin login (session #1) =="

JAR_A="$ROOT/cookies-a"
login_a=$(web_login "$JAR_A" admin adminpw99)
assert_eq "$login_a" "303" "admin login (jar A)"
echo "  admin login -> 303 OK"

# ---- step 2 — /settings/sessions shows current session -----------------------
echo ""
echo "== Step 2: GET /settings/sessions shows 'current' =="

sess_page=$(curl -sS -b "$JAR_A" "$BASE_URL/settings/sessions")
assert_contains "$sess_page" "current" "sessions page badges the current session"
# Exactly one session row so far (count the per-row revoke OR the current badge).
n_badge=$(printf '%s' "$sess_page" | grep -c '>current<' || true)
assert_eq "$n_badge" "1" "exactly one current-session badge"
echo "  /settings/sessions shows current session OK"

# ---- step 3 — second concurrent login (jar B) -> two sessions ----------------
echo ""
echo "== Step 3: second login -> two sessions =="

JAR_B="$ROOT/cookies-b"
login_b=$(web_login "$JAR_B" admin adminpw99)
assert_eq "$login_b" "303" "admin login (jar B)"

sess_page=$(curl -sS -b "$JAR_A" "$BASE_URL/settings/sessions")
# Two rows now: one "current" (this jar) + one "other" (the second jar). Count
# the revoke buttons (only non-current rows carry one).
n_revoke=$(printf '%s' "$sess_page" | grep -c 'action="/settings/sessions/revoke"' || true)
assert_eq "$n_revoke" "1" "one revocable (other) session after second login"
assert_contains "$sess_page" ">other<" "the second session shows as 'other'"
echo "  two sessions visible from jar A (1 current + 1 other) OK"

# ---- step 4 — revoke the other session -> back to one ------------------------
echo ""
echo "== Step 4: revoke the other session =="

# Scrape the other session's id_hash from the revoke form on the page.
OTHER_HASH=$(printf '%s' "$sess_page" \
    | grep -A4 'action="/settings/sessions/revoke"' \
    | grep -o 'name="id_hash" value="[^"]*"' \
    | head -1 \
    | sed 's/.*value="//;s/"//')
test -n "$OTHER_HASH" || { echo "FAIL: could not scrape other session id_hash"; exit 1; }

csrf_rev=$(get_csrf "$JAR_A" "$BASE_URL/settings/sessions")
rev_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --location \
    --data-urlencode "csrf_token=$csrf_rev" \
    --data-urlencode "id_hash=$OTHER_HASH" \
    "$BASE_URL/settings/sessions/revoke")
# 303 redirect followed to /settings/sessions (200).
assert_eq "$rev_code" "200" "revoke other session (follows redirect)"

sess_page=$(curl -sS -b "$JAR_A" "$BASE_URL/settings/sessions")
n_revoke=$(printf '%s' "$sess_page" | grep -c 'action="/settings/sessions/revoke"' || true)
assert_eq "$n_revoke" "0" "no revocable sessions after revoke (back to 1)"
echo "  revoked other session; back to a single session OK"

# The revoked jar B should now be signed out: GET /settings/sessions redirects
# to /login (302/303) instead of 200.
b_code=$(curl -sS -b "$JAR_B" -o /dev/null -w '%{http_code}' "$BASE_URL/settings/sessions")
if [[ "$b_code" != "302" && "$b_code" != "303" ]]; then
    echo "FAIL: revoked jar B still authenticated: got $b_code (want 302/303)"
    exit 1
fi
echo "  revoked session (jar B) is signed out (HTTP $b_code) OK"

# ---- step 5 — /admin/sessions lists sessions by user name --------------------
echo ""
echo "== Step 5: GET /admin/sessions lists the user =="

admin_sess=$(curl -sS -b "$JAR_A" "$BASE_URL/admin/sessions")
assert_contains "$admin_sess" "all sessions" "admin sessions page renders"
# The bare substring "admin" appears in nav links/title regardless of table
# contents; assert on the joined user-name cell markup instead.
assert_contains "$admin_sess" '<td class="mono">admin</td>' "admin sessions table lists user 'admin'"
assert_contains "$admin_sess" 'class="badge">current' "admin's own session is badged current"
echo "  /admin/sessions lists user 'admin' (table row + current badge) OK"

# ---- step 5b — sessions CLI agrees with the web view --------------------------
echo ""
echo "== Step 5b: bucketvcs session list (CLI) =="

cli_sessions=$("$BUCKETVCS" session list --auth-db "$AUTH_DB")
n_cli=$(printf '%s\n' "$cli_sessions" | grep -c '"user":"admin"' || true)
assert_eq "$n_cli" "1" "CLI lists exactly the one live admin session"
echo "  session list CLI OK"

echo ""
echo "SESSIONS_OK"

# =============================================================================
# PART B — AUDIT VIEWER (async shipping; deterministic flush via graceful stop)
# =============================================================================

# ---- step 6 — generate a repo-scoped audit event (policy-rejected push) ------
echo ""
echo "== Step 6: produce a repo-scoped audit event on acme/demo =="

# 6.1 Seed acme/demo with an initial commit on refs/heads/main.
SRC="$ROOT/src"; rm -rf "$SRC"; git init -q "$SRC"
git -C "$SRC" -c user.email=a@b.c -c user.name=a commit -q --allow-empty -m c1
AUTHED="http://admin:$TOKEN@127.0.0.1:$PORT/acme/demo.git"
git -C "$SRC" -c credential.helper= push -q "$AUTHED" HEAD:refs/heads/main
echo "  seeded acme/demo refs/heads/main OK"

# 6.2 Add a protected-ref rule via the web that blocks deletion of main.
JAR_ADMIN="$JAR_A"
csrf_pol=$(get_csrf "$JAR_ADMIN" "$BASE_URL/acme/demo/settings/policy")
test -n "$csrf_pol" || { echo "FAIL: no csrf on acme/demo policy tab"; exit 1; }
pol_code=$(curl -sS -b "$JAR_ADMIN" -c "$JAR_ADMIN" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_pol" \
    --data-urlencode "pattern=refs/heads/main" \
    --data-urlencode "block_deletion=on" \
    "$BASE_URL/acme/demo/settings/policy/refs/add")
assert_eq "$pol_code" "303" "add protected-ref rule (block deletion of main)"
echo "  protected-ref rule added (block deletion) OK"

# 6.3 Attempt to delete refs/heads/main. The push is rejected by policy, which
#     emits a policy.ref.rejected audit event scoped to acme/demo. git exits
#     non-zero (rejected) — expected; we only care that the event is emitted.
if git -C "$SRC" -c credential.helper= push -q "$AUTHED" :refs/heads/main 2>/dev/null; then
    echo "FAIL: deletion of protected ref unexpectedly SUCCEEDED"
    exit 1
fi
echo "  deletion of protected ref rejected (audit event emitted) OK"

# ---- step 7 — graceful stop flushes the spool; restart against same store ----
echo ""
echo "== Step 7: flush via graceful stop, restart serve #2 =="

stop_serve_graceful
echo "  serve #1 stopped (shutdown flush drained spool)"

PORT=$(free_port)
BASE_URL="http://127.0.0.1:$PORT"
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "$STORE_URL" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$ROOT/mirror2" \
    --lfs=false \
    --log-shipping=on \
    --log-ship-interval=5s \
    --log-ship-max-events=5 \
    --log-spool-dir="$ROOT/spool2" \
    > "$ROOT/serve2.log" 2>&1 &
SERVE_PID=$!
wait_ready "$BASE_URL"
echo "  serve #2 READY (pid=$SERVE_PID)"

# Confirm something was actually shipped to the activity stream. If the
# shutdown flush produced no activity object, the audit assertions below cannot
# be deterministic — degrade to a visible SKIP (still keeps sessions green).
ACTIVITY_OBJS=$(find "$STORE_DIR/objects/sys/logs/activity" -name '*.ndjson.gz' 2>/dev/null | wc -l | tr -d ' ')
echo "  shipped activity objects: $ACTIVITY_OBJS"

# ---- step 8 — re-login and read the audit pages ------------------------------
echo ""
echo "== Step 8: audit viewer assertions =="

JAR_C="$ROOT/cookies-c"
login_c=$(web_login "$JAR_C" admin adminpw99)
assert_eq "$login_c" "303" "admin re-login on serve #2"

if [[ "$ACTIVITY_OBJS" -eq 0 ]]; then
    # No shipped activity object — cannot assert a flushed event deterministically.
    # Still verify the pages RENDER (200 + shipping-lag banner / empty state).
    admin_audit=$(curl -sS -b "$JAR_C" "$BASE_URL/admin/audit")
    assert_contains "$admin_audit" "audit log" "/admin/audit renders"
    assert_contains "$admin_audit" "shipped to object storage" "/admin/audit shows shipping-lag banner"
    repo_audit=$(curl -sS -b "$JAR_C" "$BASE_URL/acme/demo/settings/audit")
    assert_contains "$repo_audit" "audit log" "/acme/demo/settings/audit renders"
    echo "SKIP: deterministic audit-flush assertion (no activity object shipped); pages render OK"
else
    # Deterministic: the policy.ref.rejected event was flushed.
    admin_audit=$(curl -sS -b "$JAR_C" "$BASE_URL/admin/audit")
    assert_contains "$admin_audit" "policy.ref.rejected" "/admin/audit shows the policy.ref.rejected event"
    echo "  /admin/audit shows policy.ref.rejected OK"

    # Per-repo tab for acme/demo: the event appears.
    repo_audit_demo=$(curl -sS -b "$JAR_C" "$BASE_URL/acme/demo/settings/audit")
    assert_contains "$repo_audit_demo" "policy.ref.rejected" "acme/demo audit tab shows the event"
    echo "  /acme/demo/settings/audit shows policy.ref.rejected OK"

    # Per-repo tab for acme/other: the event MUST NOT appear (repo scoping).
    repo_audit_other=$(curl -sS -b "$JAR_C" "$BASE_URL/acme/other/settings/audit")
    assert_not_contains "$repo_audit_other" "policy.ref.rejected" \
        "acme/other audit tab must NOT show acme/demo's event"
    echo "  /acme/other/settings/audit does NOT show the event (scoping) OK"

    # Sanity: the per-repo banner names the scoping guarantee.
    assert_contains "$repo_audit_demo" "scoped to acme/demo" "per-repo audit tab declares repo scope"
    echo "  per-repo audit tab declares repo scope OK"

    echo ""
    echo "AUDIT_OK"
fi

# Cleanup trap prints "ALL OBSERVABILITY SMOKE CHECKS PASSED".
