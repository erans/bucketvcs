#!/usr/bin/env bash
# scripts/smoke-oidc.sh — M22 OIDC token-exchange end-to-end (localfs).
#
# Demonstrates the full OIDC flow:
#   1. Register an OIDC issuer + a trust rule bound to org/app.
#   2. A local IdP stub signs an id_token (JWT); gateway can fetch its JWKS.
#   3. POST /_oidc/token exchanges that JWT for a bvts access token.
#   4. git push with that token to the bound repo (org/app) SUCCEEDS.
#   5. git push with the same token to a different repo (org/other) FAILS (403).
#
# Exits with SMOKE OK on success. Skips with exit 77 if go/git/curl/python3 missing.

set -euo pipefail

if ! command -v go      >/dev/null 2>&1; then echo "SKIP: go not on PATH";      exit 77; fi
if ! command -v git     >/dev/null 2>&1; then echo "SKIP: git not on PATH";     exit 77; fi
if ! command -v curl    >/dev/null 2>&1; then echo "SKIP: curl not on PATH";    exit 77; fi
if ! command -v python3 >/dev/null 2>&1; then echo "SKIP: python3 not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# ---------- port picker (same approach as m21 smoke) ----------
pick_port() {
    local i candidate inuse skip
    inuse="$(ss -ltn 2>/dev/null | awk 'NR>1 {sub(/.*:/, "", $4); print $4}' | sort -u)"
    skip="${1:-__none__}"
    for i in $(seq 1 40); do
        local seed=$(( $$ * 1000 + i + RANDOM ))
        candidate="$(awk 'BEGIN{srand('"$seed"'); print 30000+int(rand()*10000)}')"
        if [[ "$candidate" == "$skip" ]]; then continue; fi
        if ! grep -qx "$candidate" <<<"$inuse" 2>/dev/null; then
            echo "$candidate"
            return 0
        fi
    done
    echo "FAIL: could not find free port after 40 attempts" >&2
    return 1
}

IDP_PORT="$(pick_port)"
GW_PORT="$(pick_port "$IDP_PORT")"

ISS="http://127.0.0.1:$IDP_PORT"
AUD="https://bucketvcs.smoke"
TENANT="org"
REPO_APP="app"
REPO_OTHER="other"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

TMPDIR="$(mktemp -d)"
STORE="localfs:$TMPDIR/store"
AUTH_DB="$TMPDIR/auth.db"
GW_LOG="$TMPDIR/gateway.log"
IDP_LOG="$TMPDIR/idp.log"
KEEP_TMP="${KEEP_TMP:-0}"

IDP_PID=""
GW_PID=""

cleanup() {
    rc=$?
    if [[ -n "$IDP_PID" ]] && kill -0 "$IDP_PID" 2>/dev/null; then
        kill "$IDP_PID" 2>/dev/null || true
        wait "$IDP_PID" 2>/dev/null || true
    fi
    if [[ -n "$GW_PID" ]] && kill -0 "$GW_PID" 2>/dev/null; then
        kill "$GW_PID" 2>/dev/null || true
        wait "$GW_PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 && "$KEEP_TMP" != "1" ]]; then
        rm -rf "$TMPDIR"
        echo "SMOKE OK"
    else
        echo "(forensics preserved at $TMPDIR)" >&2
        echo "--- last 80 lines of gateway.log ---" >&2
        tail -80 "$GW_LOG" 2>/dev/null >&2 || true
        echo "--- last 30 lines of idp.log ---" >&2
        tail -30 "$IDP_LOG" 2>/dev/null >&2 || true
    fi
    rm -f "$BIN"
    exit "$rc"
}
on_failure() { KEEP_TMP=1; }
trap on_failure ERR
trap cleanup EXIT

# ---------- Step 1: Write + start the IdP stub ----------
#
# The stub is a tiny Go program that:
#   - Generates an RSA-2048 key at startup
#   - Serves GET /.well-known/openid-configuration → {"issuer":"<ISS>","jwks_uri":"<ISS>/jwks"}
#   - Serves GET /jwks → JWKS with the public key (kid "k1")
#   - Serves GET /sign?aud=<aud>&sub=<sub>&repo=<repo> → signed RS256 JWT
#   - Prints "READY <addr>" to stdout when listening

mkdir -p "$TMPDIR/idp"
cat >"$TMPDIR/idp/main.go" <<'GOEOF'
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func main() {
	port := os.Getenv("IDP_PORT")
	if port == "" {
		port = "0"
	}
	iss := os.Getenv("IDP_ISS")
	if iss == "" {
		log.Fatal("IDP_ISS required")
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("keygen: %v", err)
	}

	pub := jose.JSONWebKey{
		Key:       key.Public(),
		KeyID:     "k1",
		Algorithm: "RS256",
		Use:       "sig",
	}
	jwksDoc, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}})
	if err != nil {
		log.Fatalf("marshal jwks: %v", err)
	}

	sk := jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       jose.JSONWebKey{Key: key, KeyID: "k1", Algorithm: "RS256"},
	}
	signer, err := jose.NewSigner(sk, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		log.Fatalf("new signer: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, iss, iss+"/jwks")
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(jwksDoc)
	})

	mux.HandleFunc("/sign", func(w http.ResponseWriter, r *http.Request) {
		aud := r.URL.Query().Get("aud")
		sub := r.URL.Query().Get("sub")
		repo := r.URL.Query().Get("repo")
		if aud == "" || sub == "" {
			http.Error(w, "aud and sub required", http.StatusBadRequest)
			return
		}
		now := time.Now()
		expUnix := now.Add(10 * time.Minute).Unix()
		if expParam := r.URL.Query().Get("exp"); expParam != "" {
			if v, err := strconv.ParseInt(expParam, 10, 64); err == nil {
				expUnix = v
			}
		}
		claims := map[string]any{
			"iss": iss,
			"aud": aud,
			"sub": sub,
			"iat": now.Unix(),
			"exp": expUnix,
		}
		if repo != "" {
			claims["repository"] = repo
		}
		tok, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			http.Error(w, "sign error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, tok)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	_ = strconv.Itoa(actualPort) // use it
	fmt.Printf("READY http://127.0.0.1:%d\n", actualPort)
	os.Stdout.Sync()

	log.Fatal(http.Serve(ln, mux))
}
GOEOF

# Write go.mod for the stub, using the same go-jose version as the main module.
cat >"$TMPDIR/idp/go.mod" <<MODEOF
module idpstub

go 1.21

require github.com/go-jose/go-jose/v4 v4.1.4
MODEOF

# Copy go.sum entries we need. Use the main module's sum.
grep "go-jose/go-jose/v4" "$REPO_ROOT/go.sum" >"$TMPDIR/idp/go.sum" 2>/dev/null || true

echo "==> Starting IdP stub on port $IDP_PORT"
IDP_PORT="$IDP_PORT" IDP_ISS="$ISS" go run "$TMPDIR/idp/main.go" \
    >"$TMPDIR/idp.ready" 2>"$IDP_LOG" &
IDP_PID=$!

# Wait up to 10s for the stub to print READY
IDP_READY=0
for i in $(seq 1 100); do
    if grep -q "^READY " "$TMPDIR/idp.ready" 2>/dev/null; then
        IDP_READY=1
        break
    fi
    if ! kill -0 "$IDP_PID" 2>/dev/null; then
        echo "FAIL: IdP stub process died early"
        cat "$IDP_LOG" >&2
        exit 1
    fi
    sleep 0.1
done
if [[ "$IDP_READY" -ne 1 ]]; then
    echo "FAIL: IdP stub did not print READY within 10s"
    cat "$IDP_LOG" >&2
    exit 1
fi

# Verify discovery is reachable
echo "==> Verifying IdP discovery at $ISS/.well-known/openid-configuration"
for i in $(seq 1 30); do
    if curl -sf "$ISS/.well-known/openid-configuration" >/dev/null 2>&1; then
        echo "    IdP ready"
        break
    fi
    if [[ "$i" -eq 30 ]]; then
        echo "FAIL: IdP discovery not reachable after 3s"
        cat "$IDP_LOG" >&2
        exit 1
    fi
    sleep 0.1
done

# ---------- Step 2: Init repos + register issuer + rule ----------
echo "==> Init + register repos org/app and org/other"
"$BIN" init --store="$STORE" "$TENANT" "$REPO_APP"
"$BIN" repo register "$TENANT/$REPO_APP" --auth-db="$AUTH_DB" --store="$STORE" --no-init

"$BIN" init --store="$STORE" "$TENANT" "$REPO_OTHER"
"$BIN" repo register "$TENANT/$REPO_OTHER" --auth-db="$AUTH_DB" --store="$STORE" --no-init

echo "==> Register OIDC issuer (alias=local-idp) pointing to $ISS"
"$BIN" oidc issuer add \
    --auth-db="$AUTH_DB" \
    --alias=local-idp \
    --url="$ISS"

echo "==> Add trust rule: issuer=local-idp aud=$AUD → org/app repo:write (claim: repository=org/app)"
"$BIN" oidc rule add \
    --auth-db="$AUTH_DB" \
    --issuer=local-idp \
    --audience="$AUD" \
    --tenant="$TENANT" \
    --repo="$REPO_APP" \
    --scopes=repo:write \
    --ttl=15m \
    --claim "repository=$TENANT/$REPO_APP" \
    >"$TMPDIR/rule-add.out"
if ! grep -q "^id=" "$TMPDIR/rule-add.out"; then
    echo "FAIL: oidc rule add output missing id="
    cat "$TMPDIR/rule-add.out" >&2
    exit 1
fi
echo "    rule registered: $(cat "$TMPDIR/rule-add.out")"

# ---------- Step 3: Start gateway with --oidc ----------
echo "==> Starting gateway on 127.0.0.1:$GW_PORT with --oidc"
"$BIN" serve \
    --store="$STORE" \
    --auth-db="$AUTH_DB" \
    --addr="127.0.0.1:$GW_PORT" \
    --lfs=false \
    --mirror-dir="$TMPDIR/mirror" \
    --oidc=true \
    --oidc-sweep-interval=5s \
    >"$GW_LOG" 2>&1 &
GW_PID=$!

GW_URL="http://127.0.0.1:$GW_PORT"
for i in $(seq 1 50); do
    if curl -sf "$GW_URL/healthz" >/dev/null 2>&1; then
        echo "    gateway ready"
        break
    fi
    if ! kill -0 "$GW_PID" 2>/dev/null; then
        echo "FAIL: gateway died early"
        cat "$GW_LOG" >&2
        exit 1
    fi
    sleep 0.2
    if [[ "$i" -eq 50 ]]; then
        echo "FAIL: gateway not ready after 10s"
        cat "$GW_LOG" >&2
        exit 1
    fi
done

# ---------- Step 4: Mint JWT + exchange for bvts token ----------
echo "==> Minting id_token from IdP (aud=$AUD, sub=ci-runner, repository=org/app)"
JWT="$(curl -sf "$ISS/sign?aud=$AUD&sub=ci-runner&repo=$TENANT/$REPO_APP")"
if [[ -z "$JWT" ]]; then
    echo "FAIL: could not mint JWT from IdP"
    exit 1
fi
echo "    JWT length=${#JWT} chars"

echo "==> POST /_oidc/token — exchanging JWT for bvts token"
RESP="$(curl -sS -X POST "$GW_URL/_oidc/token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
    --data-urlencode "subject_token=$JWT" \
    2>"$TMPDIR/exchange.err")"
if [[ -z "$RESP" ]]; then
    echo "FAIL: empty response from /_oidc/token"
    cat "$TMPDIR/exchange.err" >&2
    echo "--- gateway.log tail ---" >&2
    tail -40 "$GW_LOG" >&2
    exit 1
fi
echo "    response: $RESP"

TOKEN="$(python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('access_token',''))" <<<"$RESP")"
if [[ -z "$TOKEN" ]]; then
    echo "FAIL: access_token missing from response: $RESP"
    tail -40 "$GW_LOG" >&2
    exit 1
fi
echo "    access_token obtained (length=${#TOKEN})"

# Verify token_type and issued_token_type in response
TTYPE="$(python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('token_type',''))" <<<"$RESP")"
if [[ "$TTYPE" != "Bearer" ]]; then
    echo "FAIL: expected token_type=Bearer got '$TTYPE'"
    exit 1
fi
echo "    token_type=Bearer confirmed"

# ---------- Step 5: git push to bound repo (org/app) must SUCCEED ----------
echo "==> Step 5: git push to $TENANT/$REPO_APP with OIDC token (must SUCCEED)"
# Set up a temp git identity in an isolated HOME to avoid credential helpers
GIT_HOME="$TMPDIR/githome"
mkdir -p "$GIT_HOME"
export HOME="$GIT_HOME"
git config --global user.email "smoke@local"
git config --global user.name "smoke"
# Disable credential helper so git never prompts
git config --global credential.helper ""

PUSH_URL="http://_oidc:$TOKEN@127.0.0.1:$GW_PORT/$TENANT/$REPO_APP.git"
OTHER_URL="http://_oidc:$TOKEN@127.0.0.1:$GW_PORT/$TENANT/$REPO_OTHER.git"

# Use `git init` + remote add instead of `git clone` because the upstream repos
# are empty (freshly initialized) and git clone of an empty repo does not create
# the working-tree directory reliably across git versions.
mkdir -p "$TMPDIR/clone-app"
cd "$TMPDIR/clone-app"
git init -q -b main
git remote add origin "$PUSH_URL"
git commit -q --allow-empty -m "oidc smoke push"
if git push -q origin main 2>"$TMPDIR/push-app.err"; then
    echo "    push to $TENANT/$REPO_APP SUCCEEDED (expected)"
else
    echo "FAIL: push to bound repo $TENANT/$REPO_APP failed"
    echo "--- push stderr ---"
    cat "$TMPDIR/push-app.err" >&2
    echo "--- gateway.log tail ---"
    tail -40 "$GW_LOG" >&2
    exit 1
fi
cd "$REPO_ROOT"

# ---------- Step 6: git push to OTHER repo (org/other) must FAIL ----------
echo "==> Step 6: git push to $TENANT/$REPO_OTHER with org/app OIDC token (must FAIL with 403)"
mkdir -p "$TMPDIR/clone-other"
cd "$TMPDIR/clone-other"
git init -q -b main
git remote add origin "$OTHER_URL"
git commit -q --allow-empty -m "oidc cross-repo push attempt"
if git push -q origin main 2>"$TMPDIR/push-other.err"; then
    echo "FAIL: push to cross-repo $TENANT/$REPO_OTHER unexpectedly SUCCEEDED"
    cat "$TMPDIR/push-other.err" >&2
    exit 1
fi
# Verify it was denied due to scope mismatch (403 / scope mismatch)
if ! grep -Eq 'scope mismatch|403|denied|forbidden' "$TMPDIR/push-other.err" 2>/dev/null; then
    echo "FAIL: cross-repo push failed but error didn't mention scope/forbidden/403"
    echo "--- push-other.err ---"
    cat "$TMPDIR/push-other.err" >&2
    exit 1
fi
echo "    push to $TENANT/$REPO_OTHER DENIED (expected: scope mismatch)"
cd "$REPO_ROOT"

# ---------- Step 7: Confirm oidc.exchanged audit in gateway log ----------
echo "==> Verifying oidc.exchanged audit event in gateway log"
sleep 0.3
if ! grep -q "oidc.exchanged" "$GW_LOG"; then
    echo "FAIL: no oidc.exchanged audit event in gateway log"
    tail -40 "$GW_LOG" >&2
    exit 1
fi
echo "    oidc.exchanged audit confirmed"

# ---------- Step 8: Expired token must be rejected with 4xx (not 200) ----------
echo "==> Step 8: Expired JWT must be rejected by /_oidc/token (invalid_token)"
# Mint a JWT with exp set 60 seconds in the past.
PAST_EXP="$(( $(date +%s) - 60 ))"
EXPIRED_JWT="$(curl -sf "$ISS/sign?aud=$AUD&sub=ci-runner&repo=$TENANT/$REPO_APP&exp=$PAST_EXP")"
if [[ -z "$EXPIRED_JWT" ]]; then
    echo "FAIL: could not mint expired JWT from IdP"
    exit 1
fi
EXPIRED_HTTP_CODE="$(curl -sS -o "$TMPDIR/expired-resp.json" -w "%{http_code}" \
    -X POST "$GW_URL/_oidc/token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
    --data-urlencode "subject_token=$EXPIRED_JWT")"
if [[ "$EXPIRED_HTTP_CODE" == "200" ]]; then
    echo "FAIL: expired JWT was accepted (got 200); want 4xx"
    cat "$TMPDIR/expired-resp.json" >&2
    exit 1
fi
# Confirm the error reason is invalid_token.
if ! python3 -c "import json,sys; d=json.loads(open('$TMPDIR/expired-resp.json').read()); \
        assert d.get('error') == 'invalid_token', repr(d)" 2>/dev/null; then
    echo "FAIL: expired JWT response did not carry error=invalid_token (HTTP $EXPIRED_HTTP_CODE)"
    cat "$TMPDIR/expired-resp.json" >&2
    exit 1
fi
echo "    expired JWT correctly rejected (HTTP $EXPIRED_HTTP_CODE, error=invalid_token)"
