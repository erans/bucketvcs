package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// --oidc-login=true without required flags → exit 2.
func TestServe_OIDCLogin_RequiresConfig(t *testing.T) {
	var out, errb bytes.Buffer
	code := runServe(context.Background(),
		[]string{"--addr", "127.0.0.1:0", "--store", "localfs:" + t.TempDir(),
			"--auth-db", t.TempDir() + "/a.db", "--mirror-dir", t.TempDir(),
			"--lfs=false", "--oidc-login=true"},
		&out, &errb)
	if code != 2 {
		t.Fatalf("exit %d, want 2; stderr=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "oidc-login") {
		t.Fatalf("stderr should mention oidc-login requirements: %s", errb.String())
	}
}
