#!/usr/bin/env bash
#
# e2e-s3-push.sh — full end-to-end test of bucketvcs against a real Amazon S3 bucket.
#
# Thin wrapper: S3-specific setup lives here; the shared flow (build → create
# repo → push → branches → LFS → fresh-clone verification) lives in
# scripts/lib/e2e-common.sh.
#
# Usage:
#   scripts/e2e-s3-push.sh [SRC_REPO] [--keep]
#     SRC_REPO   GitHub repo URL to clone as the push source
#                (default: https://github.com/octocat/Hello-World.git)
#     --keep     Do NOT purge the test repo from S3 on exit (default: purge)
#
# Reads S3_STORE (an s3:// URL) + AWS credentials from .envrc (sourced if not
# already exported). The bucket region is resolved from
# BUCKETVCS_S3_REGION/AWS_REGION, else auto-detected via the AWS CLI, else
# defaults to us-east-1. A throwaway auth DB is created per run.
#
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "$ROOT/scripts/lib/e2e-common.sh"

e2e_parse_args "$@"

# --- backend: Amazon S3 -----------------------------------------------------
if [ -z "${S3_STORE:-}" ] && [ -f "$ROOT/.envrc" ]; then
  step "Loading env from .envrc"
  # shellcheck disable=SC1091
  set -a; . "$ROOT/.envrc"; set +a
  info "sourced $ROOT/.envrc"
fi

step "Validating configuration"
# This script is S3-specific: prefer S3_STORE, ignore any gcs:// STORE from .envrc.
STORE="${S3_STORE:-${STORE:-}}"
[ -n "$STORE" ] || die "S3_STORE is not set (expected an s3:// URL, e.g. s3://my-bucket). Set it or add it to .envrc."
case "$STORE" in
  s3://*) : ;;
  *) die "this is the S3 e2e test but the store '$STORE' is not an s3:// URL." ;;
esac

# AWS credentials: static key (env), shared profile, or ambient role chain.
if [ -n "${AWS_ACCESS_KEY_ID:-}" ]; then
  [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || die "AWS_ACCESS_KEY_ID is set but AWS_SECRET_ACCESS_KEY is missing"
  info "AWS static credentials: ${AWS_ACCESS_KEY_ID:0:4}…"
elif [ -n "${AWS_PROFILE:-${BUCKETVCS_S3_PROFILE:-}}" ]; then
  info "AWS profile: ${AWS_PROFILE:-$BUCKETVCS_S3_PROFILE}"
else
  info "no static key/profile set — relying on the ambient AWS credential chain (role/IRSA)"
fi

# Region is REQUIRED for s3://. Resolve env → AWS-CLI bucket-location → default.
BUCKET="${STORE#s3://}"; BUCKET="${BUCKET%%/*}"
REGION="${BUCKETVCS_S3_REGION:-${AWS_REGION:-}}"
if [ -z "$REGION" ]; then
  if command -v aws >/dev/null 2>&1; then
    loc="$(aws s3api get-bucket-location --bucket "$BUCKET" --query 'LocationConstraint' --output text 2>/dev/null || true)"
    case "$loc" in
      ""|None|null) REGION="us-east-1" ;;   # us-east-1 reports an empty/None constraint
      *)            REGION="$loc" ;;
    esac
    info "auto-detected region for $BUCKET: $REGION"
  else
    REGION="us-east-1"
    info "no region set and aws CLI absent; defaulting to $REGION (override with AWS_REGION)"
  fi
fi
# bucketvcs reads the region from this env var (see cmd/bucketvcs/store.go).
export BUCKETVCS_S3_REGION="$REGION"
info "STORE=$STORE  REGION=$REGION"
BACKEND="S3"

e2e_run
