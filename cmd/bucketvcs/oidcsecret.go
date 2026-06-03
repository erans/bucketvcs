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
