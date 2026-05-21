#!/usr/bin/env bash
# scripts/m13.5-lfs-quota-smoke.sh
#
# End-to-end smoke for M13.5 LFS quotas against localfs:
#   1. Build the bucketvcs binary.
#   2. Init a fresh repo + authdb.
#   3. Set a 1 MiB quota for tenant acme.
#   4. Seed a 500 KiB LFS object directly (bypasses verify — the
#      counter stays at 0 because verify wasn't called).
#   5. Reconcile to capture the 500 KiB.
#   6. Verify `quota show` reports used=500KiB, limit=1MiB.
#   7. Seed another 700 KiB object and reconcile: would overshoot.
#      The reconcile updates the counter to 1200KiB. show now
#      reports over_by=176KiB (1200KiB used vs 1024KiB limit).
#   8. Clear the quota and re-show: tenant absent / unlimited.
#
# Exits with M13.5_LFS_QUOTA_SMOKE_OK on success. Skips with exit 77
# if go missing.

set -euo pipefail

if ! command -v go >/dev/null 2>&1; then echo "SKIP: go not on PATH"; exit 77; fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building bucketvcs"
BIN="$(mktemp)"
go build -o "$BIN" "$REPO_ROOT/cmd/bucketvcs"
chmod +x "$BIN"

ROOT="$(mktemp -d)"
STORE="localfs:$ROOT/store"
AUTHDB="$ROOT/auth.db"
TENANT="acme"
REPO="m135smoke"
OID1="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
KEY1="$ROOT/store/objects/tenants/$TENANT/repos/$REPO/lfs/objects/$OID1"
OID2="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
KEY2="$ROOT/store/objects/tenants/$TENANT/repos/$REPO/lfs/objects/$OID2"

cleanup() {
    rc=$?
    if [[ "$rc" -eq 0 ]]; then
        rm -rf "$ROOT"
        echo "M13.5_LFS_QUOTA_SMOKE_OK"
    else
        echo "(forensics preserved at $ROOT)" >&2
    fi
    rm -f "$BIN"
    exit "$rc"
}
trap cleanup EXIT

echo "==> Init repo"
"$BIN" init --store="$STORE" "$TENANT" "$REPO" >/dev/null

echo "==> Set quota (1 MiB)"
"$BIN" quota set --auth-db="$AUTHDB" --tenant="$TENANT" --limit=1MiB

echo "==> Seed 500 KiB object"
mkdir -p "$(dirname "$KEY1")"
dd if=/dev/zero of="$KEY1" bs=1024 count=500 status=none

echo "==> Reconcile"
"$BIN" quota reconcile --auth-db="$AUTHDB" --store="$STORE" --tenant="$TENANT" | tee "$ROOT/recon1.log"

echo "==> Show — expect used=500KiB"
"$BIN" quota show --auth-db="$AUTHDB" --tenant="$TENANT" | tee "$ROOT/show1.log"
grep -q "used=500KiB" "$ROOT/show1.log" || { echo "FAIL: expected used=500KiB"; exit 1; }
grep -q "limit=1MiB"  "$ROOT/show1.log" || { echo "FAIL: expected limit=1MiB"; exit 1; }

echo "==> Seed 700 KiB object + reconcile"
mkdir -p "$(dirname "$KEY2")"
dd if=/dev/zero of="$KEY2" bs=1024 count=700 status=none
"$BIN" quota reconcile --auth-db="$AUTHDB" --store="$STORE" --tenant="$TENANT"

echo "==> Show — expect over_by=176KiB (1200 used vs 1024 limit)"
"$BIN" quota show --auth-db="$AUTHDB" --tenant="$TENANT" | tee "$ROOT/show2.log"
grep -q "over_by=176KiB" "$ROOT/show2.log" || { echo "FAIL: expected over_by=176KiB"; exit 1; }

echo "==> Clear quota; expect 'no quota row — unlimited'"
"$BIN" quota clear --auth-db="$AUTHDB" --tenant="$TENANT"
"$BIN" quota show  --auth-db="$AUTHDB" --tenant="$TENANT" | tee "$ROOT/show3.log"
grep -q "no quota" "$ROOT/show3.log" || { echo "FAIL: expected 'no quota' after clear"; exit 1; }

echo "==> M13.5 LFS Quota smoke: OK"
