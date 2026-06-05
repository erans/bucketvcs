package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func TestServeBYOBKeyTooShort(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key.bin")
	os.WriteFile(keyFile, []byte("tooshort"), 0o600)
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	s, _ := sqlitestore.Open(dbPath)
	s.Close()
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"serve",
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--auth-db", dbPath,
		"--lfs=false",
		"--byob-encryption-key=" + keyFile,
	}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d want 2\nstderr: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "32 bytes") {
		t.Errorf("should mention 32 bytes: %s", errb.String())
	}
}
