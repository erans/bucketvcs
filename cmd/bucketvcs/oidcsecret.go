package main

import (
	"fmt"
	"os"
	"strings"
)

// resolveOIDCClientSecret returns the client secret from a file (if path != "")
// else from the env var BUCKETVCS_OIDC_LOGIN_CLIENT_SECRET, else "" (public client).
func resolveOIDCClientSecret(file string, env func(string) string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("oidc client secret file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(env("BUCKETVCS_OIDC_LOGIN_CLIENT_SECRET")), nil
}

// minOIDCHMACKeyBytes is the shortest operator-provisioned OIDC state HMAC
// key accepted. Below this, fail closed rather than run multi-node logins
// on a guessable key.
const minOIDCHMACKeyBytes = 16

// resolveOIDCHMACKey returns the OIDC login-state HMAC key from a file
// (if path != "") else from BUCKETVCS_OIDC_HMAC_KEY. provisioned=false
// means the caller should fall back to boot-generated ephemeral entropy
// (single-instance logins only; restarts invalidate in-flight flows).
func resolveOIDCHMACKey(file string, env func(string) string) (key []byte, provisioned bool, err error) {
	var raw string
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, false, fmt.Errorf("oidc hmac key file: %w", err)
		}
		raw = strings.TrimSpace(string(b))
	} else {
		raw = strings.TrimSpace(env("BUCKETVCS_OIDC_HMAC_KEY"))
	}
	if raw == "" {
		return nil, false, nil
	}
	if len(raw) < minOIDCHMACKeyBytes {
		return nil, false, fmt.Errorf("oidc hmac key too short: %d bytes, need >= %d",
			len(raw), minOIDCHMACKeyBytes)
	}
	return []byte(raw), true, nil
}
