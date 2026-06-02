package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServe_UIEnabled_SharedListener(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a server")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	dbPath := filepath.Join(dir, "auth.db")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runServeWithListener(ctx, []string{
		"--store", "localfs:" + storeDir,
		"--auth-db", dbPath,
		"--mirror-dir", filepath.Join(dir, "mirror"),
		// --lfs requires --proxied-url-* flags (M13.1); disable here.
		"--lfs=false",
	}, io.Discard, io.Discard, ln)

	deadline := time.Now().Add(5 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get(url + "/login")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "log in") {
		t.Fatalf("GET /login: status %d body=%s", resp.StatusCode, string(body))
	}
}
