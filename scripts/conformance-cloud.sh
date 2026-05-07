#!/usr/bin/env bash
# scripts/conformance-cloud.sh
# Run the M5 ship-gate: live conformance + diffharness against R2 and
# (optionally) AWS S3. Skips a provider when its env is unset.
#
# Required for R2:
#   BUCKETVCS_R2_BUCKET, BUCKETVCS_R2_ENDPOINT, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
# Required for S3:
#   BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY

set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Running in-tree tests (no creds needed)"
go test ./internal/storage/s3compat/... -count=1

if [[ -n "${BUCKETVCS_R2_BUCKET:-}" && -n "${BUCKETVCS_R2_ENDPOINT:-}" ]]; then
  echo "==> R2 conformance"
  go test ./internal/storage/s3compat/... -run TestConformance_R2 -v -count=1

  echo "==> R2 diffharness"
  BUCKETVCS_DIFFHARNESS_STORE="r2://${BUCKETVCS_R2_BUCKET}/diffharness-$$" \
    go test ./internal/diffharness/... -count=1
else
  echo "==> R2: SKIPPED (BUCKETVCS_R2_BUCKET / BUCKETVCS_R2_ENDPOINT unset)"
fi

if [[ -n "${BUCKETVCS_S3_BUCKET:-}" && -n "${BUCKETVCS_S3_REGION:-}" ]]; then
  echo "==> S3 conformance"
  go test ./internal/storage/s3compat/... -run TestConformance_S3 -v -count=1
else
  echo "==> S3: SKIPPED (BUCKETVCS_S3_BUCKET / BUCKETVCS_S3_REGION unset)"
fi

echo "==> Cloud conformance completed"
