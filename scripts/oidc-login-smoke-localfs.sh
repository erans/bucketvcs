#!/usr/bin/env bash
# scripts/oidc-login-smoke-localfs.sh — OIDC CLI surface (no live IdP).
#
# Exercises the operator-facing CLI added for OIDC browser login: pre-creating
# a user with a verified email, updating that email, and listing a user's
# pinned identities (empty until a real browser login pins one). It does NOT
# stand up an IdP — the full RS256 verifier round-trip is covered by the Go
# e2e test (internal/web/oidc_e2e_test.go, TestOIDC_EndToEnd).
set -euo pipefail

ROOT="$(mktemp -d)"
DB="$ROOT/auth.db"
BIN="$ROOT/bucketvcs"
trap 'rm -rf "$ROOT"' EXIT

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
( cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/bucketvcs )

# Pre-create a user with a verified email (TOFU match key).
"$BIN" user add alice --email alice@corp.com --auth-db "$DB"

# Update the verified email.
"$BIN" user set-email alice alice2@corp.com --auth-db "$DB"

# No login has happened yet, so the identity list is empty — exit 0.
"$BIN" user identity list alice --auth-db "$DB"

echo "ALL OIDC CLI SMOKE CHECKS PASSED"
