#!/usr/bin/env bash
# scripts/smoke-triggers-ui.sh
#
# End-to-end smoke test for the Build Triggers web UI against a localfs gateway.
# Drives curl through admin bootstrap, web login (session cookie + CSRF), and the
# repo-settings TRIGGERS tab: list (empty), add a generic trigger (secret-once),
# add a codebuild trigger referencing an operator-configured connector, list
# (both present), render the deliveries sub-page, rename via edit, then
# disable + remove and confirm the list is empty again.
#
# Requires no cloud credentials. Dependencies: curl, go, python3 (and git only
# for the optional fire step, which SKIPs cleanly if git is unavailable).
#
# Run: ./scripts/smoke-triggers-ui.sh
# Prints a per-step OK/SKIP line and exits 0 on success.

set -euo pipefail

ROOT="$(mktemp -d)"
echo "smoke root: $ROOT"

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== TRIGGERS-UI SMOKE FAILED (status=$cleanup_status) ===="
        echo "Preserved root for forensics: $ROOT"
        if [[ -f "$ROOT/serve.log" ]]; then
            echo "==== serve.log (last 50 lines) ===="
            tail -50 "$ROOT/serve.log" || true
        fi
        [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
        return
    fi
    [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$ROOT"
    echo "ALL TRIGGERS-UI SMOKE CHECKS PASSED"
}
trap cleanup EXIT

# ---- helpers -----------------------------------------------------------------

# get_csrf <cookie_jar> <url>
# Fetches <url> with the given cookie jar, extracts the first csrf_token hidden
# field value, and prints it. Writes response cookies back to the jar via -c.
get_csrf() {
    local jar="$1" url="$2"
    curl -sS -b "$jar" -c "$jar" "$url" \
        | grep -o 'name="csrf_token" value="[^"]*"' \
        | head -1 \
        | sed 's/.*value="//;s/"//'
}

# assert_eq <got> <want> <msg>
assert_eq() {
    local got="$1" want="$2" msg="$3"
    if [[ "$got" != "$want" ]]; then
        echo "FAIL: $msg: got=$got want=$want"
        exit 1
    fi
}

# assert_contains <body> <pattern> <msg>
assert_contains() {
    local body="$1" pat="$2" msg="$3"
    if ! printf '%s' "$body" | grep -q "$pat"; then
        echo "FAIL: $msg: pattern '$pat' not found"
        exit 1
    fi
}

# ---- step 1 — build + CLI setup ----------------------------------------------
echo ""
echo "== Step 1: build + CLI setup =="

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUCKETVCS="$ROOT/bucketvcs"
( cd "$REPO_ROOT" && go build -o "$BUCKETVCS" ./cmd/bucketvcs )

STORE_DIR="$ROOT/store"; mkdir -p "$STORE_DIR"
AUTH_DB="$ROOT/auth.db"
MIRROR_DIR="$ROOT/mirror"
BUILD_CONFIG="$ROOT/build.yaml"

# Operator build-config defining one AWS connector named "prod". The codebuild
# trigger created in step 6 references this connector; the connector dropdown in
# the UI is populated from these names.
cat > "$BUILD_CONFIG" <<'YAML'
build:
  defaults:
    token_ttl: 15m
    token_scopes:
      - repo:read
      - lfs:read
  aws_connectors:
    prod:
      region: us-east-1
      profile: ""
YAML

# Create an admin user via CLI (--admin makes a global admin).
"$BUCKETVCS" user add admin --admin --auth-db "$AUTH_DB"
echo "adminpw99" | "$BUCKETVCS" user set-password admin --auth-db "$AUTH_DB" --password-stdin

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
BASE_URL="http://127.0.0.1:$PORT"

# ---- boot server (build triggers ENABLED) ------------------------------------
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "localfs:$STORE_DIR" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$MIRROR_DIR" \
    --lfs=false \
    --ui=true \
    --build-triggers=true \
    --build-config="$BUILD_CONFIG" \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!

for i in $(seq 1 50); do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break
    sleep 0.1
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "server did not start"; exit 1; }
echo "  OK: server READY (pid=$SERVE_PID), build triggers enabled"

# ---- step 2 — admin login ----------------------------------------------------
echo ""
echo "== Step 2: admin login =="

JAR="$ROOT/cookies-admin"
csrf_login=$(get_csrf "$JAR" "$BASE_URL/login")
test -n "$csrf_login" || { echo "FAIL: no csrf on login page"; exit 1; }

login_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=admin" \
    --data-urlencode "password=adminpw99" \
    --data-urlencode "csrf_token=$csrf_login" \
    --data-urlencode "next=/" \
    "$BASE_URL/login")
assert_eq "$login_code" "303" "admin login"
echo "  OK: admin login -> 303"

# ---- step 3 — register repo acme/demo ----------------------------------------
echo ""
echo "== Step 3: register repo acme/demo =="

csrf_repos=$(get_csrf "$JAR" "$BASE_URL/admin/repos")
test -n "$csrf_repos" || { echo "FAIL: no csrf on /admin/repos"; exit 1; }

reg_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_repos" \
    --data-urlencode "tenant=acme" \
    --data-urlencode "name=demo" \
    "$BASE_URL/admin/repos/register")
assert_eq "$reg_code" "303" "register acme/demo"
echo "  OK: register acme/demo -> 303"

# ---- step 4 — triggers tab is empty ------------------------------------------
echo ""
echo "== Step 4: GET triggers tab (empty) =="

TRIGGERS_URL="$BASE_URL/acme/demo/settings/triggers"
triggers_page=$(curl -sS -b "$JAR" -c "$JAR" "$TRIGGERS_URL")
assert_contains "$triggers_page" "no triggers configured" "triggers tab shows empty state"
echo "  OK: triggers tab -> 'no triggers configured'"

# ---- step 5 — add a generic trigger (secret-once) ----------------------------
echo ""
echo "== Step 5: add generic trigger (secret shown once) =="

csrf_add=$(get_csrf "$JAR" "$BASE_URL/acme/demo/settings/triggers/new")
test -n "$csrf_add" || { echo "FAIL: no csrf on triggers/new"; exit 1; }

gen_body=$(curl -sS -b "$JAR" -c "$JAR" \
    --data-urlencode "csrf_token=$csrf_add" \
    --data-urlencode "name=ci-generic" \
    --data-urlencode "kind=generic" \
    --data-urlencode "url=https://ci.example.invalid/hook" \
    --data-urlencode "token_mode=none" \
    "$BASE_URL/acme/demo/settings/triggers/add")
# generic kind auto-generates a secret -> rendered on the secret-once page.
assert_contains "$gen_body" "will not be shown again" "generic add returns secret-once page"
echo "  OK: generic trigger add -> secret-once page"

# ---- step 6 — add a codebuild trigger referencing the connector --------------
echo ""
echo "== Step 6: add codebuild trigger (connector=prod) =="

csrf_add2=$(get_csrf "$JAR" "$BASE_URL/acme/demo/settings/triggers/new?kind=codebuild")
test -n "$csrf_add2" || { echo "FAIL: no csrf on triggers/new?kind=codebuild"; exit 1; }

cb_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
    --location \
    --data-urlencode "csrf_token=$csrf_add2" \
    --data-urlencode "name=ci-codebuild" \
    --data-urlencode "kind=codebuild" \
    --data-urlencode "aws_connector=prod" \
    --data-urlencode "aws_region=us-east-1" \
    --data-urlencode "aws_project=demo-project" \
    --data-urlencode "token_mode=none" \
    "$BASE_URL/acme/demo/settings/triggers/add")
# codebuild has no auto-generated secret -> 303 redirect to the triggers tab.
assert_eq "$cb_code" "200" "codebuild add (303 -> 200 follow)"
echo "  OK: codebuild trigger add -> 303 -> 200"

# ---- step 7 — list shows both triggers ---------------------------------------
echo ""
echo "== Step 7: triggers tab lists both =="

list_page=$(curl -sS -b "$JAR" -c "$JAR" "$TRIGGERS_URL")
assert_contains "$list_page" "ci-generic" "list contains ci-generic"
assert_contains "$list_page" "ci-codebuild" "list contains ci-codebuild"
echo "  OK: list contains 'ci-generic' and 'ci-codebuild'"

# Extract the generic trigger's id from its deliveries link for later steps.
# Each row links: .../triggers/deliveries?trigger=<bvbt_...>
GEN_ID=$(printf '%s' "$list_page" \
    | grep -o 'triggers/deliveries?trigger=bvbt_[A-Za-z0-9_-]*' \
    | head -1 | sed 's/.*trigger=//')
test -n "$GEN_ID" || { echo "FAIL: could not extract a trigger id from the list page"; exit 1; }
echo "  OK: extracted trigger id ${GEN_ID:0:14}..."

# ---- step 8 — deliveries page renders (fire step SKIPped) --------------------
echo ""
echo "== Step 8: deliveries page renders =="

# A real push to fire a trigger requires git + a full clone/commit/push cycle and
# a reachable receiver; that is out of scope for this UI smoke. We SKIP the
# fire+poll step but still assert the deliveries sub-page renders even when empty.
echo "  SKIP: push-to-fire + delivery poll (too heavy for a UI smoke; needs git + reachable receiver)"

deliveries_page=$(curl -sS -b "$JAR" -c "$JAR" \
    "$BASE_URL/acme/demo/settings/triggers/deliveries?trigger=$GEN_ID")
assert_contains "$deliveries_page" "deliveries" "deliveries page has a deliveries heading"
assert_contains "$deliveries_page" "no deliveries found" "deliveries page shows empty state"
echo "  OK: deliveries page renders (empty)"

# ---- step 9 — edit (rename) --------------------------------------------------
echo ""
echo "== Step 9: rename ci-generic via edit =="

csrf_edit=$(get_csrf "$JAR" "$BASE_URL/acme/demo/settings/triggers/new?id=$GEN_ID")
test -n "$csrf_edit" || { echo "FAIL: no csrf on triggers/new?id="; exit 1; }

edit_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
    --location \
    --data-urlencode "csrf_token=$csrf_edit" \
    --data-urlencode "id=$GEN_ID" \
    --data-urlencode "name=ci-generic-renamed" \
    --data-urlencode "kind=generic" \
    --data-urlencode "token_mode=none" \
    --data-urlencode "active=on" \
    "$BASE_URL/acme/demo/settings/triggers/edit")
assert_eq "$edit_code" "200" "edit rename (303 -> 200 follow)"

list_after_edit=$(curl -sS -b "$JAR" -c "$JAR" "$TRIGGERS_URL")
assert_contains "$list_after_edit" "ci-generic-renamed" "list shows renamed trigger"
echo "  OK: rename -> list shows 'ci-generic-renamed'"

# ---- step 10 — disable then remove both, list empties ------------------------
echo ""
echo "== Step 10: disable + remove both triggers =="

# Collect every trigger id currently shown.
ALL_IDS=$(printf '%s' "$list_after_edit" \
    | grep -o 'triggers/deliveries?trigger=bvbt_[A-Za-z0-9_-]*' \
    | sed 's/.*trigger=//' | sort -u)
test -n "$ALL_IDS" || { echo "FAIL: no trigger ids found before disable/remove"; exit 1; }

for id in $ALL_IDS; do
    csrf_d=$(get_csrf "$JAR" "$TRIGGERS_URL")
    dis_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
        --location \
        --data-urlencode "csrf_token=$csrf_d" \
        --data-urlencode "id=$id" \
        "$BASE_URL/acme/demo/settings/triggers/disable")
    assert_eq "$dis_code" "200" "disable $id (303 -> 200)"

    csrf_r=$(get_csrf "$JAR" "$TRIGGERS_URL")
    rem_code=$(curl -sS -b "$JAR" -c "$JAR" -o /dev/null -w '%{http_code}' \
        --location \
        --data-urlencode "csrf_token=$csrf_r" \
        --data-urlencode "id=$id" \
        "$BASE_URL/acme/demo/settings/triggers/remove")
    assert_eq "$rem_code" "200" "remove $id (303 -> 200)"
done
echo "  OK: disabled + removed all triggers"

final_page=$(curl -sS -b "$JAR" -c "$JAR" "$TRIGGERS_URL")
assert_contains "$final_page" "no triggers configured" "list is empty after removal"
echo "  OK: triggers tab back to 'no triggers configured'"

# Cleanup trap prints "ALL TRIGGERS-UI SMOKE CHECKS PASSED".
