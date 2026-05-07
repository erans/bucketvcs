#!/usr/bin/env bash
# scripts/conformance-cloud.sh
# Run the M5 ship-gate: live conformance + diffharness against R2 and
# (optionally) AWS S3. Skips a provider when its env is unset.
#
# Required for R2 (provider-specific override OR generic):
#   BUCKETVCS_R2_BUCKET, BUCKETVCS_R2_ENDPOINT
#   BUCKETVCS_R2_ACCESS_KEY_ID, BUCKETVCS_R2_SECRET_ACCESS_KEY  (preferred)
#     or AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY  (fallback)
#
# Required for S3 (provider-specific override OR generic):
#   BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION
#   BUCKETVCS_S3_ACCESS_KEY_ID, BUCKETVCS_S3_SECRET_ACCESS_KEY  (preferred)
#     or AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY  (fallback)
#
# Operators running BOTH providers in one invocation should use the
# provider-specific overrides so each test sees the right credentials.

set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Running in-tree tests (no creds needed)"
go test ./internal/storage/s3compat/... -count=1

# R2: resolve creds with provider-specific override.
r2_key="${BUCKETVCS_R2_ACCESS_KEY_ID:-${AWS_ACCESS_KEY_ID:-}}"
r2_secret="${BUCKETVCS_R2_SECRET_ACCESS_KEY:-${AWS_SECRET_ACCESS_KEY:-}}"

if [[ -z "${BUCKETVCS_R2_BUCKET:-}" || -z "${BUCKETVCS_R2_ENDPOINT:-}" ]]; then
  echo "==> R2: SKIPPED (BUCKETVCS_R2_BUCKET / BUCKETVCS_R2_ENDPOINT unset)"
elif [[ -z "$r2_key" || -z "$r2_secret" ]]; then
  echo "==> R2: SKIPPED (R2 credentials unset; set BUCKETVCS_R2_ACCESS_KEY_ID/BUCKETVCS_R2_SECRET_ACCESS_KEY or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)"
else
  echo "==> R2 conformance"
  AWS_ACCESS_KEY_ID="$r2_key" AWS_SECRET_ACCESS_KEY="$r2_secret" \
    go test ./internal/storage/s3compat/... -run TestConformance_R2 -v -count=1

  echo "==> R2 diffharness"
  AWS_ACCESS_KEY_ID="$r2_key" AWS_SECRET_ACCESS_KEY="$r2_secret" \
  BUCKETVCS_DIFFHARNESS_STORE="r2://${BUCKETVCS_R2_BUCKET}/diffharness-$$" \
    go test ./internal/diffharness/... -count=1
fi

# S3: resolve creds with provider-specific override.
s3_key="${BUCKETVCS_S3_ACCESS_KEY_ID:-${AWS_ACCESS_KEY_ID:-}}"
s3_secret="${BUCKETVCS_S3_SECRET_ACCESS_KEY:-${AWS_SECRET_ACCESS_KEY:-}}"

if [[ -z "${BUCKETVCS_S3_BUCKET:-}" || -z "${BUCKETVCS_S3_REGION:-}" ]]; then
  echo "==> S3: SKIPPED (BUCKETVCS_S3_BUCKET / BUCKETVCS_S3_REGION unset)"
elif [[ -z "$s3_key" || -z "$s3_secret" ]]; then
  echo "==> S3: SKIPPED (S3 credentials unset; set BUCKETVCS_S3_ACCESS_KEY_ID/BUCKETVCS_S3_SECRET_ACCESS_KEY or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)"
else
  echo "==> S3 conformance"
  AWS_ACCESS_KEY_ID="$s3_key" AWS_SECRET_ACCESS_KEY="$s3_secret" \
    go test ./internal/storage/s3compat/... -run TestConformance_S3 -v -count=1
fi

echo "==> Cloud conformance completed"
