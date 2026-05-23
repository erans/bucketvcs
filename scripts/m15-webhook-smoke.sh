#!/usr/bin/env bash
# scripts/m15-webhook-smoke.sh
#
# End-to-end smoke for M15 webhooks against localfs:
#   1. Build the bucketvcs binary + a tiny Go HTTP receiver sidecar.
#   2. Init a fresh repo + authdb. Create user 'alice' + token + write grant.
#   3. Start `bucketvcs serve` (with the webhook worker auto-booted).
#   4. Register a webhook endpoint subscribing to all events.
#   5. Push an initial commit; assert the receiver records a push event
#      with a non-empty BucketVCS-Signature header.
#   6. Add a protected_refs rule + force-push; assert the receiver records
#      a policy.ref.rejected event.
#   7. Sanity-check that the gateway log emits webhooks_delivery_total
#      and webhooks.delivered audit lines.
#
# Exits with `M15_WEBHOOK_SMOKE_OK` on success. Skips with exit 77 if
# go / git / curl is missing.

set -euo pipefail

if ! command -v go   >/dev/null 2>&1; then echo "SKIP: go not on PATH"; exit 77; fi
if ! command -v git  >/dev/null 2>&1; then echo "SKIP: git not on PATH"; exit 77; fi
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
REPO="m15smoke"
PORT="$(awk 'BEGIN{srand(); print 30000+int(rand()*10000)}')"
RECEIVER_PORT="$(awk 'BEGIN{srand(); print 40000+int(rand()*10000)}')"
URL="http://127.0.0.1:$PORT"
RECEIVED_LOG="$ROOT/received.log"
: >"$RECEIVED_LOG"

PID=""
RECEIVER_PID=""
cleanup() {
    rc=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    if [[ -n "$RECEIVER_PID" ]] && kill -0 "$RECEIVER_PID" 2>/dev/null; then
        kill "$RECEIVER_PID" 2>/dev/null || true
        wait "$RECEIVER_PID" 2>/dev/null || true
    fi
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M15_WEBHOOK_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT; gateway.log + received.log)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

# ---- Sidecar receiver ----
cat >"$ROOT/receiver.go" <<'EOF'
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	logf, err := os.Create(os.Getenv("RECEIVED_LOG"))
	if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	defer logf.Close()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(logf, "event=%s sig=%s delivery=%s body=%s\n",
			r.Header.Get("X-BucketVCS-Event"),
			r.Header.Get("BucketVCS-Signature"),
			r.Header.Get("X-BucketVCS-Delivery-ID"),
			string(body))
		logf.Sync()
		w.WriteHeader(http.StatusOK)
	})
	addr := os.Getenv("LISTEN")
	if addr == "" { addr = ":18080" }
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "receiver: %v\n", err)
		os.Exit(1)
	}
}
EOF
echo "==> Building receiver sidecar"
go build -o "$ROOT/receiver" "$ROOT/receiver.go"

echo "==> Init + register repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO"
"$BIN" repo register "$TENANT/$REPO" --auth-db="$AUTHDB" --store="$STORE" --no-init

echo "==> Create user + token + grant"
"$BIN" user add alice --auth-db="$AUTHDB"
"$BIN" repo grant alice "$TENANT/$REPO" write --auth-db="$AUTHDB"
ALICE_TOKEN=$("$BIN" token create alice --auth-db="$AUTHDB" | grep -m1 '^bvts_')
if [[ -z "$ALICE_TOKEN" ]]; then echo "FAIL: could not extract alice token"; exit 1; fi

echo "==> Start receiver sidecar on :$RECEIVER_PORT"
RECEIVED_LOG="$RECEIVED_LOG" LISTEN=":$RECEIVER_PORT" "$ROOT/receiver" \
    >"$ROOT/receiver.log" 2>&1 &
RECEIVER_PID=$!
for i in $(seq 1 50); do
    if curl -s -o /dev/null "http://127.0.0.1:$RECEIVER_PORT/" 2>/dev/null; then break; fi
    if ! kill -0 "$RECEIVER_PID" 2>/dev/null; then echo "FAIL: receiver died"; cat "$ROOT/receiver.log"; exit 1; fi
    sleep 0.1
done

echo "==> Register webhook endpoint"
"$BIN" webhook endpoint add \
    --auth-db="$AUTHDB" \
    --tenant="$TENANT" --repo="$REPO" \
    --url="http://127.0.0.1:$RECEIVER_PORT/" \
    --events=all >"$ROOT/endpoint-add.out"
if ! grep -q "secret=" "$ROOT/endpoint-add.out"; then
    echo "FAIL: no secret in endpoint add output"
    cat "$ROOT/endpoint-add.out" >&2
    exit 1
fi
echo "    endpoint registered: $(head -1 "$ROOT/endpoint-add.out")"

echo "==> Start gateway on $URL"
"$BIN" serve --store="$STORE" --auth-db="$AUTHDB" --addr="127.0.0.1:$PORT" --lfs=false \
    --mirror-dir="$ROOT/mirror" \
    >"$ROOT/gateway.log" 2>&1 &
PID=$!

for i in $(seq 1 50); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$PID" 2>/dev/null; then echo "FAIL: gateway died early"; cat "$ROOT/gateway.log"; exit 1; fi
    sleep 0.2
done

CLONE_URL="http://alice:$ALICE_TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

echo "==> Step 5: push initial commit to refs/heads/main"
WORK="$ROOT/work"
git init -q -b main "$WORK"
(
    cd "$WORK"
    git config user.email t@example.com
    git config user.name t
    git commit -q --allow-empty -m "initial"
    git push -q "$CLONE_URL" main:refs/heads/main
)
C1="$(cd "$WORK" && git rev-parse HEAD)"
echo "    initial commit $C1"

echo "==> Wait up to 10s for receiver to record push event"
for i in $(seq 1 100); do
    if grep -q "event=push" "$RECEIVED_LOG" 2>/dev/null; then break; fi
    sleep 0.1
done
if ! grep -q "event=push" "$RECEIVED_LOG" 2>/dev/null; then
    echo "FAIL: no push event after 10s"
    echo "--- received.log ---"; cat "$RECEIVED_LOG" 2>/dev/null || true
    echo "--- gateway.log tail ---"; tail -50 "$ROOT/gateway.log" 2>/dev/null || true
    exit 1
fi
if ! grep -q "sig=t=" "$RECEIVED_LOG"; then
    echo "FAIL: push event missing BucketVCS-Signature with t= prefix"
    cat "$RECEIVED_LOG" >&2
    exit 1
fi
echo "    push event observed with signature"

echo "==> Step 6: add protected_refs rule + attempt force-push"
"$BIN" policy refs add --auth-db="$AUTHDB" --tenant="$TENANT" --repo="$REPO" \
    --pattern=refs/heads/main
# Advance the server with a FF commit first so a reset-and-recommit becomes a
# true non-FF rewrite (and therefore triggers the protected_refs rule).
(
    cd "$WORK"
    git commit -q --allow-empty -m "ff"
    git push -q "$CLONE_URL" main:refs/heads/main
)
(
    cd "$WORK"
    git reset --hard "$C1" -q
    git commit -q --allow-empty -m "alt"
    if git push --force "$CLONE_URL" main:refs/heads/main 2>"$ROOT/forcepush.err"; then
        echo "FAIL: force-push was accepted under protected rule"
        cat "$ROOT/forcepush.err" >&2
        exit 1
    fi
)
echo "    force-push rejected (expected)"

echo "==> Wait up to 10s for receiver to record policy.ref.rejected event"
for i in $(seq 1 100); do
    if grep -q "event=policy.ref.rejected" "$RECEIVED_LOG" 2>/dev/null; then break; fi
    sleep 0.1
done
if ! grep -q "event=policy.ref.rejected" "$RECEIVED_LOG" 2>/dev/null; then
    echo "FAIL: no policy.ref.rejected event after 10s"
    echo "--- received.log ---"; cat "$RECEIVED_LOG" 2>/dev/null || true
    echo "--- gateway.log tail ---"; tail -50 "$ROOT/gateway.log" 2>/dev/null || true
    exit 1
fi
echo "    policy.ref.rejected event observed"

echo "==> Step 7: sanity-check observability"
# Wait a tick for the worker to flush metrics + audit through slog default.
sleep 1
if ! grep -Eq 'name="?webhooks_delivery_total"?' "$ROOT/gateway.log"; then
    echo "FAIL: gateway log missing webhooks_delivery_total metric"
    tail -50 "$ROOT/gateway.log" >&2
    exit 1
fi
if ! grep -Eq 'webhooks\.delivered' "$ROOT/gateway.log"; then
    echo "FAIL: gateway log missing webhooks.delivered audit event"
    tail -50 "$ROOT/gateway.log" >&2
    exit 1
fi
echo "    metrics + audit observed"

echo "==> M15 webhooks smoke: OK"
