#!/usr/bin/env bash
# scripts/m13.4-lfs-gc-smoke.sh
#
# End-to-end smoke for M13.4 LFS GC against localfs:
#   1. Build the bucketvcs binary.
#   2. Init a fresh repo.
#   3. Seed an orphan LFS object directly via the store (no real LFS push needed).
#   4. Run `bucketvcs gc --lfs --retention=1s` -> mark + sweep.
#   5. Assert the orphan is gone.
#   6. Seed a second orphan and run with --dry-run; assert it survives.
#
# Exits with `M13.4_LFS_GC_SMOKE_OK` on success. Skips with exit 77 if
# go is missing.

set -euo pipefail

if ! command -v go >/dev/null 2>&1; then echo "SKIP: go not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE="localfs:$ROOT/store"
TENANT="acme"
REPO="m134smoke"
ORPHAN_OID="deadbeef00000000000000000000000000000000000000000000000000000000"
ORPHAN_KEY="$ROOT/store/objects/tenants/$TENANT/repos/$REPO/lfs/objects/$ORPHAN_OID"
SECOND_OID="cafebabe00000000000000000000000000000000000000000000000000000000"
SECOND_KEY="$ROOT/store/objects/tenants/$TENANT/repos/$REPO/lfs/objects/$SECOND_OID"

cleanup() {
    rc=$?
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M13.4_LFS_GC_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO" >/dev/null

echo "==> Seed orphan LFS object directly"
mkdir -p "$(dirname "$ORPHAN_KEY")"
printf 'fake lfs body\n' > "$ORPHAN_KEY"
if [[ ! -f "$ORPHAN_KEY" ]]; then echo "FAIL: seed missing"; exit 1; fi

# Phase 1: --mark-only persists a mark with first_seen_unreferenced_at = now.
echo "==> Phase 1: gc --lfs --mark-only --retention=1s"
"$BIN" gc --store="$STORE" --repo="$TENANT/$REPO" --lfs --mark-only --retention=1s 2>&1 | tee "$ROOT/gc-mark.log" | tail -5

# Sleep so the orphan ages past the 1s retention floor.
sleep 2

# Phase 2: --sweep-only deletes the now-eligible orphan.
echo "==> Phase 2: gc --lfs --sweep-only --retention=1s"
"$BIN" gc --store="$STORE" --repo="$TENANT/$REPO" --lfs --sweep-only --retention=1s 2>&1 | tee "$ROOT/gc-sweep.log" | tail -5

echo "==> Assert orphan gone"
if [[ -f "$ORPHAN_KEY" ]]; then
    echo "FAIL: orphan still present at $ORPHAN_KEY"
    exit 1
fi
echo "    orphan deleted"

echo "==> Seed a second orphan + gc --lfs --dry-run; assert it survives"
printf 'second\n' > "$SECOND_KEY"
# Mark + sweep in one combined dry-run pass. The orphan is brand new
# so first_seen_unreferenced_at = now; sleep so it ages past 1s.
"$BIN" gc --store="$STORE" --repo="$TENANT/$REPO" --lfs --mark-only --retention=1s >/dev/null
sleep 2
"$BIN" gc --store="$STORE" --repo="$TENANT/$REPO" --lfs --sweep-only --retention=1s --dry-run 2>&1 | tee "$ROOT/gc-dryrun.log" | tail -5

if [[ ! -f "$SECOND_KEY" ]]; then
    echo "FAIL: --dry-run deleted the object"
    exit 1
fi
echo "    dry-run preserved object"

# Phase 3: --sweep-only without --dry-run finally reclaims it. Assert.
echo "==> Phase 3: real sweep reclaims the second orphan"
"$BIN" gc --store="$STORE" --repo="$TENANT/$REPO" --lfs --sweep-only --retention=1s >/dev/null
if [[ -f "$SECOND_KEY" ]]; then
    echo "FAIL: second orphan still present after real sweep"
    exit 1
fi
echo "    second orphan deleted"

echo "==> M13.4 LFS GC smoke: OK"
