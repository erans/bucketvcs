#!/usr/bin/env bash
# scripts/web-ui-phase3-smoke-localfs.sh
#
# End-to-end smoke test for the M24 Phase 3 web UI settings & admin feature
# against a localfs gateway. Drives curl through admin user/repo management,
# repo settings (public toggle, access grant, webhook add, protected-ref add),
# token creation and git auth, quota set, CSRF enforcement, tier authz checks,
# and repo delete.
#
# Requires no cloud credentials. Dependencies: curl, go, git, python3.

set -euo pipefail

ROOT="$(mktemp -d)"
echo "smoke root: $ROOT"

SERVE_PID=""
cleanup_status=0
cleanup() {
    cleanup_status=$?
    if [[ $cleanup_status -ne 0 ]]; then
        echo "==== PHASE 3 SMOKE FAILED (status=$cleanup_status) ===="
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
    echo "ALL PHASE 3 SETTINGS/ADMIN SMOKE CHECKS PASSED"
}
trap cleanup EXIT

# ---- helpers -----------------------------------------------------------------

# get_csrf <cookie_jar> <url>
# Fetches <url> with the given cookie jar, extracts the first csrf_token hidden
# field value, and prints it. Writes the response cookies back to the jar via -c.
get_csrf() {
    local jar="$1" url="$2"
    curl -sS -b "$jar" -c "$jar" "$url" \
        | grep -o 'name="csrf_token" value="[^"]*"' \
        | head -1 \
        | sed 's/.*value="//;s/"//'
}

# post_form <cookie_jar> <url> [extra curl args...]
# Issues a POST with the cookie jar, following redirects (--location), and
# returns the HTTP final status code.
post_form() {
    local jar="$1" url="$2"; shift 2
    curl -sS -b "$jar" -c "$jar" -o /dev/null -w '%{http_code}' \
        --location \
        "$@" \
        "$url"
}

# assert_eq <got> <want> <msg>
assert_eq() {
    local got="$1" want="$2" msg="$3"
    if [[ "$got" != "$want" ]]; then
        echo "FAIL: $msg: got=$got want=$want"
        exit 1
    fi
}

# assert_contains <file_or_text> <pattern> <msg>
# Reads stdin if first arg is "-".
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

# Create users via CLI. --admin flag makes a global admin.
"$BUCKETVCS" user add admin --admin --auth-db "$AUTH_DB"
echo "adminpw99" | "$BUCKETVCS" user set-password admin --auth-db "$AUTH_DB" --password-stdin
"$BUCKETVCS" user add alice --auth-db "$AUTH_DB"
echo "alicepw99" | "$BUCKETVCS" user set-password alice --auth-db "$AUTH_DB" --password-stdin

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
BASE_URL="http://127.0.0.1:$PORT"

# ---- boot server -------------------------------------------------------------
"$BUCKETVCS" serve \
    --addr "127.0.0.1:$PORT" \
    --store "localfs:$STORE_DIR" \
    --auth-db "$AUTH_DB" \
    --mirror-dir "$MIRROR_DIR" \
    --lfs=false \
    > "$ROOT/serve.log" 2>&1 &
SERVE_PID=$!

for i in $(seq 1 50); do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break
    sleep 0.1
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "server did not start"; exit 1; }
echo "  server READY (pid=$SERVE_PID)"

# ---- step 2 — login admin ----------------------------------------------------
echo ""
echo "== Step 2: login admin =="

JAR_A="$ROOT/cookies-admin"
csrf_login=$(get_csrf "$JAR_A" "$BASE_URL/login")
test -n "$csrf_login" || { echo "FAIL: no csrf on login page"; exit 1; }

login_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=admin" \
    --data-urlencode "password=adminpw99" \
    --data-urlencode "csrf_token=$csrf_login" \
    --data-urlencode "next=/" \
    "$BASE_URL/login")
assert_eq "$login_code" "303" "admin login"
echo "  admin login 303 OK"

# ---- step 3 — POST /admin/users/create (bob) ---------------------------------
echo ""
echo "== Step 3: admin create user bob =="

csrf_admin_users=$(get_csrf "$JAR_A" "$BASE_URL/admin/users")
test -n "$csrf_admin_users" || { echo "FAIL: no csrf on /admin/users"; exit 1; }

create_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_admin_users" \
    --data-urlencode "name=bob" \
    --data-urlencode "password=" \
    "$BASE_URL/admin/users/create")
assert_eq "$create_code" "303" "admin user create bob"
echo "  create bob -> 303 OK"

users_page=$(curl -sS -b "$JAR_A" "$BASE_URL/admin/users")
assert_contains "$users_page" "bob" "admin/users contains bob"
echo "  /admin/users contains 'bob' OK"

# ---- step 4 — POST /admin/repos/register acme/demo --------------------------
echo ""
echo "== Step 4: admin register repo acme/demo =="

csrf_admin_repos=$(get_csrf "$JAR_A" "$BASE_URL/admin/repos")
test -n "$csrf_admin_repos" || { echo "FAIL: no csrf on /admin/repos"; exit 1; }

reg_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_admin_repos" \
    --data-urlencode "tenant=acme" \
    --data-urlencode "name=demo" \
    "$BASE_URL/admin/repos/register")
assert_eq "$reg_code" "303" "admin repo register"
echo "  register acme/demo -> 303 OK"

# Repo settings page accessible to admin.
settings_code=$(curl -sS -b "$JAR_A" -o /dev/null -w '%{http_code}' "$BASE_URL/acme/demo/settings")
assert_eq "$settings_code" "200" "GET /acme/demo/settings after register"
echo "  /acme/demo/settings -> 200 OK"

# Storage: the root manifest file exists under the store (layout:
# objects/tenants/<tenant>/repos/<repo>/manifest/root.json).
manifest_file=$(find "$STORE_DIR" -name "root.json" | head -1)
test -n "$manifest_file" || { echo "FAIL: no root.json manifest found under $STORE_DIR after register"; exit 1; }
echo "  storage manifest exists at $manifest_file OK"

# ---- step 5 — grant alice admin on acme/demo --------------------------------
echo ""
echo "== Step 5: grant alice admin on acme/demo =="

csrf_access=$(get_csrf "$JAR_A" "$BASE_URL/acme/demo/settings/access")
test -n "$csrf_access" || { echo "FAIL: no csrf on /acme/demo/settings/access"; exit 1; }

grant_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_access" \
    --data-urlencode "username=alice" \
    --data-urlencode "perm=admin" \
    "$BASE_URL/acme/demo/settings/access/grant")
assert_eq "$grant_code" "303" "grant alice admin"
echo "  grant alice admin -> 303 OK"

# ---- step 6 — login alice and access repo settings --------------------------
echo ""
echo "== Step 6: login alice; access /acme/demo/settings =="

JAR_B="$ROOT/cookies-alice"
csrf_alice_login=$(get_csrf "$JAR_B" "$BASE_URL/login")
test -n "$csrf_alice_login" || { echo "FAIL: no csrf on login for alice"; exit 1; }

alice_login=$(curl -sS -b "$JAR_B" -c "$JAR_B" -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=alice" \
    --data-urlencode "password=alicepw99" \
    --data-urlencode "csrf_token=$csrf_alice_login" \
    --data-urlencode "next=/" \
    "$BASE_URL/login")
assert_eq "$alice_login" "303" "alice login"
echo "  alice login 303 OK"

alice_settings=$(curl -sS -b "$JAR_B" -o /dev/null -w '%{http_code}' "$BASE_URL/acme/demo/settings")
assert_eq "$alice_settings" "200" "alice GET /acme/demo/settings"
echo "  alice: /acme/demo/settings -> 200 OK (repo-admin perm works)"

# ---- step 7 — alice makes acme/demo public; anonymous can see it -------------
echo ""
echo "== Step 7: alice makes repo public; anonymous GET /acme/demo =="

csrf_alice_general=$(get_csrf "$JAR_B" "$BASE_URL/acme/demo/settings")
test -n "$csrf_alice_general" || { echo "FAIL: no csrf on /acme/demo/settings for alice"; exit 1; }

public_code=$(curl -sS -b "$JAR_B" -c "$JAR_B" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_alice_general" \
    --data-urlencode "public=on" \
    "$BASE_URL/acme/demo/settings/public")
assert_eq "$public_code" "303" "alice set public=on"
echo "  set public -> 303 OK"

# Anonymous access — no cookie jar.
anon_code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/acme/demo")
assert_eq "$anon_code" "200" "anonymous GET /acme/demo after public=on"
echo "  anonymous /acme/demo -> 200 OK"

# ---- step 8 — alice adds a webhook endpoint ---------------------------------
echo ""
echo "== Step 8: alice adds webhook endpoint =="

csrf_alice_wh=$(get_csrf "$JAR_B" "$BASE_URL/acme/demo/settings/webhooks")
test -n "$csrf_alice_wh" || { echo "FAIL: no csrf on webhooks tab for alice"; exit 1; }

wh_body=$(curl -sS -b "$JAR_B" -c "$JAR_B" \
    --data-urlencode "csrf_token=$csrf_alice_wh" \
    --data-urlencode "url=https://example.invalid/hook" \
    --data-urlencode "events=all" \
    "$BASE_URL/acme/demo/settings/webhooks/add")
assert_contains "$wh_body" "will not be shown again" "webhook add returns secret-once page"
echo "  webhook add: secret-once page rendered OK"

# ---- step 9 — alice adds a protected ref rule --------------------------------
echo ""
echo "== Step 9: alice adds protected ref rule (refs/heads/main) =="

csrf_alice_pol=$(get_csrf "$JAR_B" "$BASE_URL/acme/demo/settings/policy")
test -n "$csrf_alice_pol" || { echo "FAIL: no csrf on policy tab for alice"; exit 1; }

pol_code=$(curl -sS -b "$JAR_B" -c "$JAR_B" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_alice_pol" \
    --data-urlencode "pattern=refs/heads/main" \
    --data-urlencode "block_deletion=on" \
    "$BASE_URL/acme/demo/settings/policy/refs/add")
assert_eq "$pol_code" "303" "alice add protected ref rule"
echo "  add protected ref rule -> 303 OK"

policy_page=$(curl -sS -b "$JAR_B" "$BASE_URL/acme/demo/settings/policy")
assert_contains "$policy_page" "refs/heads/main" "policy page contains refs/heads/main after add"
echo "  policy page contains 'refs/heads/main' OK"

# ---- step 10 — tier checks: alice cannot access /admin or hooks tab ----------
echo ""
echo "== Step 10: tier authz checks =="

admin_code=$(curl -sS -b "$JAR_B" -o /dev/null -w '%{http_code}' "$BASE_URL/admin")
assert_eq "$admin_code" "404" "alice GET /admin returns 404"
echo "  alice: /admin -> 404 OK (non-admin)"

hooks_code=$(curl -sS -b "$JAR_B" -o /dev/null -w '%{http_code}' "$BASE_URL/acme/demo/settings/hooks")
assert_eq "$hooks_code" "404" "alice GET /acme/demo/settings/hooks returns 404"
echo "  alice: /acme/demo/settings/hooks -> 404 OK (non-admin)"

# ---- step 11 — alice creates an API token and uses it for git ls-remote ------
echo ""
echo "== Step 11: alice creates API token; git ls-remote =="

csrf_alice_tok=$(get_csrf "$JAR_B" "$BASE_URL/settings/tokens")
test -n "$csrf_alice_tok" || { echo "FAIL: no csrf on /settings/tokens for alice"; exit 1; }

tok_body=$(curl -sS -b "$JAR_B" -c "$JAR_B" \
    --data-urlencode "csrf_token=$csrf_alice_tok" \
    --data-urlencode "scopes=repo:read,repo:write" \
    --data-urlencode "label=smoke-test" \
    "$BASE_URL/settings/tokens/create")
assert_contains "$tok_body" "bvts_" "token create response contains bvts_ prefix"
assert_contains "$tok_body" "will not be shown again" "token create returns secret-once page"
echo "  token create: secret-once page with bvts_ OK"

# Extract the token (it appears once inside a <pre> block).
TOKEN=$(printf '%s' "$tok_body" | grep -o 'bvts_[A-Z0-9_]*' | head -1)
test -n "$TOKEN" || { echo "FAIL: could not extract token from response body"; exit 1; }
echo "  extracted token: ${TOKEN:0:12}... OK"

# git ls-remote using token as password (username can be anything; bucketvcs
# validates by token secret). The repo has no branches yet (registered but
# never pushed to), so ls-remote should exit 0 with empty output.
GIT_TERMINAL_PROMPT=0 git -c credential.helper= \
    ls-remote "http://alice:$TOKEN@127.0.0.1:$PORT/acme/demo.git" \
    >/dev/null 2>&1 \
    || { echo "FAIL: git ls-remote with API token failed"; exit 1; }
echo "  git ls-remote with token OK"

# ---- step 12 — admin sets quota for acme ------------------------------------
echo ""
echo "== Step 12: admin sets quota for tenant acme =="

csrf_admin_q=$(get_csrf "$JAR_A" "$BASE_URL/admin/quotas")
test -n "$csrf_admin_q" || { echo "FAIL: no csrf on /admin/quotas"; exit 1; }

q_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf_token=$csrf_admin_q" \
    --data-urlencode "tenant=acme" \
    --data-urlencode "limit=10GiB" \
    "$BASE_URL/admin/quotas/set")
assert_eq "$q_code" "303" "admin quota set"
echo "  quota set -> 303 OK"

quotas_page=$(curl -sS -b "$JAR_A" "$BASE_URL/admin/quotas")
assert_contains "$quotas_page" "acme" "quotas page contains tenant acme after set"
echo "  /admin/quotas contains 'acme' OK"

# ---- step 13 — CSRF negative: POST without csrf_token returns 403 -----------
echo ""
echo "== Step 13: CSRF negative check =="

# We need the bvcs_csrf cookie set (get /settings/tokens), but we deliberately
# omit the csrf_token POST field. The handler must return 403.
# Fetch the page to plant the bvcs_csrf cookie in JAR_B, then POST without the field.
curl -sS -b "$JAR_B" -c "$JAR_B" "$BASE_URL/settings/tokens" >/dev/null

nocsrf_code=$(curl -sS -b "$JAR_B" -c "$JAR_B" -o /dev/null -w '%{http_code}' \
    --data-urlencode "scopes=repo:read" \
    --data-urlencode "label=nocsrf" \
    "$BASE_URL/settings/tokens/create")
assert_eq "$nocsrf_code" "403" "token create without csrf_token field returns 403"
echo "  token create without CSRF -> 403 OK"

# ---- step 14 — admin deletes acme/demo --------------------------------------
echo ""
echo "== Step 14: admin deletes acme/demo =="

csrf_del=$(get_csrf "$JAR_A" "$BASE_URL/acme/demo/settings")
test -n "$csrf_del" || { echo "FAIL: no csrf on /acme/demo/settings for delete"; exit 1; }

del_code=$(curl -sS -b "$JAR_A" -c "$JAR_A" -o /dev/null -w '%{http_code}' \
    --location \
    --data-urlencode "csrf_token=$csrf_del" \
    --data-urlencode "confirm=acme/demo" \
    "$BASE_URL/acme/demo/settings/delete")
# After delete we're redirected to "/" (303 -> 200).
assert_eq "$del_code" "200" "admin repo delete (follows redirect to /)"
echo "  delete acme/demo -> 303 -> 200 OK"

gone_code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/acme/demo")
assert_eq "$gone_code" "404" "GET /acme/demo after delete returns 404"
echo "  /acme/demo after delete -> 404 OK"

# Cleanup trap prints "ALL PHASE 3 SETTINGS/ADMIN SMOKE CHECKS PASSED".
