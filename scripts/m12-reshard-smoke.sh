#!/usr/bin/env bash
# scripts/m12-reshard-smoke.sh
#
# End-to-end smoke for M12 ref sharding against localfs:
#   1. Build the bucketvcs binary.
#   2. Seed a bare git repo with 101 refs.
#   3. Import it into a fresh repo under localfs:<tmp>.
#   4. inspect-manifest --json: assert v1 (refs >= 101, ref_shards == 0).
#   5. Run `bucketvcs reshard-refs` and assert outcome == success.
#   6. inspect-manifest --json: assert v2 (refs == 0, ref_shards >= 1,
#      ref_sharding == "hash_v1").
#   7. Re-run reshard-refs; assert outcome == noop.
#   8. Export the v2 repo and assert every ref present.
#   9. Tear down.
#
# Skips with exit 77 if Go toolchain or git is unavailable.

set -euo pipefail

if ! command -v go >/dev/null 2>&1; then
    echo "SKIP: go not on PATH"
    exit 77
fi
if ! command -v git >/dev/null 2>&1; then
    echo "SKIP: git not on PATH"
    exit 77
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "SKIP: python3 not on PATH"
    exit 77
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs binary"
ROOT="$(mktemp -d)"
BIN="$ROOT/bucketvcs"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"

STORE="localfs:$ROOT/store"
TENANT="acme"
REPO="m12smoke"

cleanup() {
    rc=$?
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M12_RESHARD_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)"
    fi
    exit "$rc"
}
trap cleanup EXIT

echo "==> Seed a bare git repo with 101 refs"
SEED="$ROOT/seed"
git init -q --bare "$SEED"
WORK="$ROOT/work"
git init -q -b main "$WORK"
(
    cd "$WORK"
    git config user.email smoke@example.com
    git config user.name smoke
    echo seed > README
    git add README
    git commit -qm initial
    git remote add origin "$SEED"
    git push -q origin main:refs/heads/main
    # Create 100 additional refs all pointing at the same commit to
    # exercise the sharding distribution.
    SHA=$(git rev-parse HEAD)
    for i in $(seq 1 100); do
        git push -q origin "$SHA:refs/heads/branch-$i"
    done
)

echo "==> Import seed into bucketvcs repo"
"$BIN" import --store="$STORE" --default-branch=refs/heads/main "$SEED" "$TENANT" "$REPO"

echo "==> Inspect pre-reshard manifest"
PRE=$("$BIN" inspect-manifest --store="$STORE" --json "$TENANT" "$REPO")
REFCOUNT_PRE=$(echo "$PRE" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("refs",{})))')
SHARDS_PRE=$(echo "$PRE" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("ref_shards",[])))')
if [[ "$REFCOUNT_PRE" -lt 101 ]]; then
    echo "FAIL: pre-reshard refs count = $REFCOUNT_PRE (expected >= 101)"
    exit 1
fi
if [[ "$SHARDS_PRE" -ne 0 ]]; then
    echo "FAIL: pre-reshard expected empty ref_shards, got $SHARDS_PRE"
    exit 1
fi
echo "    pre-reshard refs=$REFCOUNT_PRE shards=$SHARDS_PRE (v1 confirmed)"

echo "==> Run reshard-refs"
RESHARD=$("$BIN" reshard-refs --store="$STORE" --repo="$TENANT/$REPO" --json)
echo "    $RESHARD"
OUTCOME=$(echo "$RESHARD" | python3 -c 'import sys,json;print(json.load(sys.stdin)["outcome"])')
if [[ "$OUTCOME" != "success" ]]; then
    echo "FAIL: reshard outcome = $OUTCOME (expected success)"
    exit 1
fi

echo "==> Inspect post-reshard manifest"
POST=$("$BIN" inspect-manifest --store="$STORE" --json "$TENANT" "$REPO")
REFCOUNT_POST=$(echo "$POST" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("refs",{})))')
SHARDS_POST=$(echo "$POST" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(len(b.get("ref_shards",[])))')
SHARDING_POST=$(echo "$POST" | python3 -c 'import sys,json;b=json.load(sys.stdin);print(b.get("ref_sharding",""))')
if [[ "$REFCOUNT_POST" -ne 0 ]]; then
    echo "FAIL: post-reshard refs count = $REFCOUNT_POST (expected 0)"
    exit 1
fi
if [[ "$SHARDS_POST" -lt 1 ]]; then
    echo "FAIL: post-reshard ref_shards count = $SHARDS_POST (expected >= 1)"
    exit 1
fi
if [[ "$SHARDING_POST" != "hash_v1" ]]; then
    echo "FAIL: post-reshard ref_sharding = $SHARDING_POST (expected hash_v1)"
    exit 1
fi
echo "    post-reshard refs=$REFCOUNT_POST shards=$SHARDS_POST sharding=$SHARDING_POST (v2 confirmed)"

echo "==> Re-run reshard-refs; expect noop"
NOOP=$("$BIN" reshard-refs --store="$STORE" --repo="$TENANT/$REPO" --json)
OUTCOME2=$(echo "$NOOP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["outcome"])')
if [[ "$OUTCOME2" != "noop" ]]; then
    echo "FAIL: second reshard outcome = $OUTCOME2 (expected noop)"
    exit 1
fi
echo "    noop confirmed"

echo "==> Export the v2 repo and assert every ref present"
DEST="$ROOT/export"
"$BIN" export --store="$STORE" "$TENANT" "$REPO" "$DEST"
EXPORTED_REFS=$(cd "$DEST" && git for-each-ref --format='%(refname)' | wc -l)
if [[ "$EXPORTED_REFS" -lt 101 ]]; then
    echo "FAIL: exported refs = $EXPORTED_REFS (expected >= 101)"
    exit 1
fi
echo "    exported $EXPORTED_REFS refs from v2 repo"

echo "M12 reshard smoke: OK"
