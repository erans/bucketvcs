#!/usr/bin/env bash
#
# e2e-gcs-push.sh — full end-to-end test of bucketvcs against a real GCS bucket.
#
# Thin wrapper: GCS-specific setup lives here; the shared flow (build → create
# repo → push → branches → LFS → fresh-clone verification) lives in
# scripts/lib/e2e-common.sh.
#
# Usage:
#   scripts/e2e-gcs-push.sh [SRC_REPO] [--keep]
#     SRC_REPO   GitHub repo URL to clone as the push source
#                (default: https://github.com/octocat/Hello-World.git)
#     --keep     Do NOT purge the test repo from GCS on exit (default: purge)
#
# Reads STORE (a gcs:// URL) + GCS credentials from .envrc (sourced if not
# already exported). A throwaway auth DB is created per run.
#
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "$ROOT/scripts/lib/e2e-common.sh"

e2e_parse_args "$@"

# --- backend: Google Cloud Storage -----------------------------------------
if [ -z "${STORE:-}" ] && [ -f "$ROOT/.envrc" ]; then
  step "Loading env from .envrc"
  # shellcheck disable=SC1091
  set -a; . "$ROOT/.envrc"; set +a
  info "sourced $ROOT/.envrc"
fi

step "Validating configuration"
[ -n "${STORE:-}" ] || die "STORE is not set (expected a gcs:// URL, e.g. gcs://my-bucket). Set it or add it to .envrc."
case "$STORE" in
  gcs://*) : ;;
  *) die "this is the GCS e2e test but STORE=$STORE is not a gcs:// URL." ;;
esac
# GCS auth uses Application Default Credentials. Locally that means a key file
# in GOOGLE_APPLICATION_CREDENTIALS or the bucketvcs alias.
CRED="${BUCKETVCS_GCS_CREDENTIALS_FILE:-${GOOGLE_APPLICATION_CREDENTIALS:-}}"
if [ -n "$CRED" ]; then
  [ -f "$CRED" ] || die "GCS credential file not found: $CRED"
  info "GCS credentials: $CRED"
else
  info "no key file set — relying on ambient Application Default Credentials (keyless)"
fi
info "STORE=$STORE"
BACKEND="GCS"

e2e_run
