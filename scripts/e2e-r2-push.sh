#!/usr/bin/env bash
#
# e2e-r2-push.sh — full end-to-end test of bucketvcs against a real Cloudflare R2 bucket.
#
# Thin wrapper: R2-specific setup lives here; the shared flow (build → create
# repo → push → branches → LFS → fresh-clone verification) lives in
# scripts/lib/e2e-common.sh.
#
# Usage:
#   scripts/e2e-r2-push.sh [SRC_REPO] [--keep]
#     SRC_REPO   GitHub repo URL to clone as the push source
#                (default: https://github.com/octocat/Hello-World.git)
#     --keep     Do NOT purge the test repo from R2 on exit (default: purge)
#
# Reads R2_STORE from .envrc (sourced if not already exported). R2_STORE may be
# either:
#   - an r2:// URL (then BUCKETVCS_S3_ENDPOINT must also be set), or
#   - the full S3 API URL https://<account-id>.r2.cloudflarestorage.com/<bucket>
#     (the wrapper splits it into the r2:// store + BUCKETVCS_S3_ENDPOINT).
#
# Credentials: R2 API token (Object Read & Write), via R2_ACCESS_KEY_ID /
# R2_SECRET_ACCESS_KEY (preferred — keeps them distinct from your AWS keys), or
# AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY. The r2:// scheme applies region=auto
# and path-style automatically, so no region is needed.
#
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "$ROOT/scripts/lib/e2e-common.sh"

e2e_parse_args "$@"

# --- backend: Cloudflare R2 -------------------------------------------------
if [ -z "${R2_STORE:-}" ] && [ -f "$ROOT/.envrc" ]; then
  step "Loading env from .envrc"
  # shellcheck disable=SC1091
  set -a; . "$ROOT/.envrc"; set +a
  info "sourced $ROOT/.envrc"
fi

step "Validating configuration"
[ -n "${R2_STORE:-}" ] || die "R2_STORE is not set (expected r2://<bucket> or https://<account>.r2.cloudflarestorage.com/<bucket>). Set it or add it to .envrc."

# Normalise R2_STORE into an r2:// STORE + BUCKETVCS_S3_ENDPOINT.
case "$R2_STORE" in
  r2://*)
    STORE="$R2_STORE"
    [ -n "${BUCKETVCS_S3_ENDPOINT:-}" ] \
      || die "r2:// store requires BUCKETVCS_S3_ENDPOINT (the https://<account>.r2.cloudflarestorage.com endpoint)"
    ;;
  http://*|https://*)
    proto="${R2_STORE%%://*}"
    rest="${R2_STORE#*://}"          # <host>[/<bucket>[/prefix]]
    host="${rest%%/*}"               # <account-id>.r2.cloudflarestorage.com
    if [ "$host" = "$rest" ]; then
      # Endpoint only (no bucket in the URL): take the bucket from R2_BUCKET,
      # defaulting to the same name the S3/GCS e2e tests use.
      bucketpath="${R2_BUCKET:-bucketvcs-test1}"
    else
      bucketpath="${rest#*/}"        # <bucket>[/prefix]
    fi
    [ -n "$bucketpath" ] || die "could not determine R2 bucket; set R2_BUCKET or include it in R2_STORE"
    export BUCKETVCS_S3_ENDPOINT="$proto://$host"
    STORE="r2://$bucketpath"
    info "parsed R2_STORE → endpoint=$BUCKETVCS_S3_ENDPOINT  store=$STORE"
    ;;
  *)
    die "R2_STORE must be an r2:// URL or an https://<account>.r2.cloudflarestorage.com/<bucket> URL (got: $R2_STORE)"
    ;;
esac

# R2 credentials. Prefer R2_* so they don't collide with AWS keys used by the
# S3 test; bucketvcs reads them through the standard AWS_* variables.
if [ -n "${R2_ACCESS_KEY_ID:-}" ] && [ -n "${R2_SECRET_ACCESS_KEY:-}" ]; then
  export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
  export AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
  unset AWS_SESSION_TOKEN AWS_PROFILE 2>/dev/null || true
  info "R2 credentials: R2_ACCESS_KEY_ID ${R2_ACCESS_KEY_ID:0:4}…"
elif [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
  info "using AWS_ACCESS_KEY_ID ${AWS_ACCESS_KEY_ID:0:4}… (must be your R2 API token, NOT an AWS key)"
else
  die "no R2 credentials: set R2_ACCESS_KEY_ID + R2_SECRET_ACCESS_KEY (preferred) or AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY to your R2 API token"
fi

info "STORE=$STORE  ENDPOINT=${BUCKETVCS_S3_ENDPOINT:-<unset>}"
BACKEND="R2"

e2e_run
