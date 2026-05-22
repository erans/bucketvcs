#!/usr/bin/env bash
# scripts/m14-policy-smoke.sh
#
# End-to-end smoke for M14 protected refs against localfs:
#   1. Build the bucketvcs binary.
#   2. Init a fresh repo + authdb. Create user 'alice' + token + write grant.
#   3. Start `bucketvcs serve` with the authdb wired (policy.Service is
#      always constructed when --auth-db is in play).
#   4. Push an initial commit to refs/heads/main via real git client.
#   5. Add a protected_refs rule blocking deletion + force-push on
#      refs/heads/main.
#   6. Push a fast-forward commit -> assert ACCEPT.
#   7. Force-push -> assert REJECT with "protected-branch" reason.
#   8. Branch deletion -> assert REJECT.
#   9. Remove the rule + retry force-push -> assert ACCEPT.
#
# Exits with `M14_POLICY_SMOKE_OK` on success. Skips with exit 77 if
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
REPO="m14smoke"
PORT="$(awk 'BEGIN{srand(); print 30000+int(rand()*10000)}')"
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
        echo "M14_POLICY_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT; logs at $ROOT/gateway.log)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init + register repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO"
"$BIN" repo register "$TENANT/$REPO" --auth-db="$AUTHDB" --store="$STORE" --no-init

echo "==> Create user + token + grant"
"$BIN" user add alice --auth-db="$AUTHDB"
"$BIN" repo grant alice "$TENANT/$REPO" write --auth-db="$AUTHDB"
ALICE_TOKEN=$("$BIN" token create alice --auth-db="$AUTHDB" | grep -m1 '^bvts_')
if [[ -z "$ALICE_TOKEN" ]]; then echo "FAIL: could not extract alice token"; exit 1; fi

echo "==> Start gateway on $URL"
# M14 has no LFS dependency; --lfs=false avoids M13.1's hard-require of
# --proxied-url-signing-key + --proxied-url-base.
"$BIN" serve --store="$STORE" --auth-db="$AUTHDB" --addr="127.0.0.1:$PORT" --lfs=false \
    --mirror-dir="$ROOT/mirror" \
    >"$ROOT/gateway.log" 2>&1 &
PID=$!

# Wait for the gateway to bind.
for i in $(seq 1 50); do
    if curl -sf "$URL/healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$PID" 2>/dev/null; then echo "FAIL: gateway died early"; cat "$ROOT/gateway.log"; exit 1; fi
    sleep 0.2
done

CLONE_URL="http://alice:$ALICE_TOKEN@127.0.0.1:$PORT/$TENANT/$REPO.git"

echo "==> Push initial commit to refs/heads/main"
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
echo "    initial commit $C1 on refs/heads/main"

echo "==> Add a protected_refs rule"
"$BIN" policy refs add --auth-db="$AUTHDB" --tenant="$TENANT" --repo="$REPO" \
    --pattern=refs/heads/main

echo "==> Step 6: push a fast-forward commit (expect ACCEPT)"
(
    cd "$WORK"
    git commit -q --allow-empty -m "ff"
    git push "$CLONE_URL" main:refs/heads/main 2>"$ROOT/ff.err"
)
echo "    FF accepted"

echo "==> Step 7: attempt a force-push (expect REJECT)"
(
    cd "$WORK"
    git reset --hard "$C1" -q
    git commit -q --allow-empty -m "alt"
    if git push --force "$CLONE_URL" main:refs/heads/main 2>"$ROOT/forcepush.err"; then
        echo "FAIL: force-push was accepted"
        cat "$ROOT/forcepush.err" >&2
        exit 1
    fi
)
if ! grep -q "protected-branch" "$ROOT/forcepush.err"; then
    echo "FAIL: force-push error missing protected-branch reason"
    cat "$ROOT/forcepush.err" >&2
    exit 1
fi
echo "    Force-push rejected with: $(grep -o 'protected-branch[^]]*' "$ROOT/forcepush.err" | head -1)"

echo "==> Step 8: attempt a deletion (expect REJECT)"
(
    cd "$WORK"
    if git push "$CLONE_URL" :refs/heads/main 2>"$ROOT/delete.err"; then
        echo "FAIL: deletion was accepted"
        cat "$ROOT/delete.err" >&2
        exit 1
    fi
)
if ! grep -q "protected-branch" "$ROOT/delete.err"; then
    echo "FAIL: deletion error missing protected-branch reason"
    cat "$ROOT/delete.err" >&2
    exit 1
fi
echo "    Deletion rejected with: $(grep -o 'protected-branch[^]]*' "$ROOT/delete.err" | head -1)"

echo "==> Step 9: remove the rule + retry force-push (expect ACCEPT)"
"$BIN" policy refs remove --auth-db="$AUTHDB" --tenant="$TENANT" --repo="$REPO" \
    --pattern=refs/heads/main
(
    cd "$WORK"
    git push --force "$CLONE_URL" main:refs/heads/main 2>"$ROOT/forcepush2.err"
)
echo "    Force-push accepted after rule removal"

echo "==> Sanity check: gateway log emitted policy metrics + audit"
# slog's default text handler emits unquoted key=value pairs;
# accept both the text and JSON shapes for forward-compat.
if ! grep -Eq 'metric_name="?policy_refs_check_total"?' "$ROOT/gateway.log"; then
    echo "FAIL: gateway log missing policy_refs_check_total metric"
    tail -50 "$ROOT/gateway.log" >&2
    exit 1
fi
if ! grep -Eq 'event="?policy\.ref\.rejected"?' "$ROOT/gateway.log"; then
    echo "FAIL: gateway log missing policy.ref.rejected audit event"
    tail -50 "$ROOT/gateway.log" >&2
    exit 1
fi
echo "    metrics + audit observed"

echo "==> M14 protected-refs smoke: OK"
