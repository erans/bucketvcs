#!/usr/bin/env bash
#
# e2e-azure-push.sh — full end-to-end test of bucketvcs against a real Azure Blob container.
#
# Thin wrapper: Azure-specific setup lives here; the shared flow (build → create
# repo → push → branches → LFS → fresh-clone verification) lives in
# scripts/lib/e2e-common.sh.
#
# Usage:
#   scripts/e2e-azure-push.sh [SRC_REPO] [--keep]
#     SRC_REPO   GitHub repo URL to clone as the push source
#                (default: https://github.com/octocat/Hello-World.git)
#     --keep     Do NOT purge the test repo from Azure on exit (default: purge)
#
# Reads AZURE_STORE (an azureblob://<container> URL) + the BUCKETVCS_AZURE_*
# auth vars from .envrc (sourced if not already exported). bucketvcs reads the
# credentials straight from those env vars; supply ONE of:
#   - BUCKETVCS_AZURE_CONNECTION_STRING, or
#   - BUCKETVCS_AZURE_ACCOUNT + BUCKETVCS_AZURE_ACCOUNT_KEY, or
#   - BUCKETVCS_AZURE_SERVICE_URL (+ account) for DefaultAzureCredential.
#
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "$ROOT/scripts/lib/e2e-common.sh"

e2e_parse_args "$@"

# --- backend: Azure Blob Storage --------------------------------------------
if [ -z "${AZURE_STORE:-}" ] && [ -f "$ROOT/.envrc" ]; then
  step "Loading env from .envrc"
  # shellcheck disable=SC1091
  set -a; . "$ROOT/.envrc"; set +a
  info "sourced $ROOT/.envrc"
fi

step "Validating configuration"
# This script is Azure-specific: prefer AZURE_STORE, ignore any other STORE.
STORE="${AZURE_STORE:-${STORE:-}}"
[ -n "$STORE" ] || die "AZURE_STORE is not set (expected azureblob://<container>, e.g. azureblob://bucketvcs-test1). Set it or add it to .envrc."
case "$STORE" in
  azureblob://*) : ;;
  *) die "this is the Azure e2e test but the store '$STORE' is not an azureblob:// URL." ;;
esac

# Azure auth: the bucketvcs binary reads these BUCKETVCS_AZURE_* vars directly
# (no remapping). Validate that at least one auth path is configured, and that
# the credentials are NOT in the URL (the adapter rejects that).
if [ -n "${BUCKETVCS_AZURE_CONNECTION_STRING:-}" ]; then
  info "Azure auth: connection string"
elif [ -n "${BUCKETVCS_AZURE_ACCOUNT:-}" ] && [ -n "${BUCKETVCS_AZURE_ACCOUNT_KEY:-}" ]; then
  info "Azure auth: account + shared key (${BUCKETVCS_AZURE_ACCOUNT})"
elif [ -n "${BUCKETVCS_AZURE_SERVICE_URL:-}" ] || [ -n "${BUCKETVCS_AZURE_ACCOUNT:-}" ]; then
  info "Azure auth: DefaultAzureCredential (service URL / account; no shared key)"
else
  die "no Azure credentials: set BUCKETVCS_AZURE_CONNECTION_STRING, or BUCKETVCS_AZURE_ACCOUNT + BUCKETVCS_AZURE_ACCOUNT_KEY, or BUCKETVCS_AZURE_SERVICE_URL (for DefaultAzureCredential)."
fi

info "STORE=$STORE  ACCOUNT=${BUCKETVCS_AZURE_ACCOUNT:-<from-connection-string>}"
BACKEND="Azure"

e2e_run
