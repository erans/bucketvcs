package main

import (
	"path/filepath"
	"testing"
)

func TestResolveHostKey_FlagWins(t *testing.T) {
	got, err := resolveHostKey("/explicit/key", &envLookup{
		BUCKETVCS_SSH_HOST_KEY: "/env/key",
		XDG_STATE_HOME:         "/xdg",
		HOME:                   "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/key" {
		t.Fatalf("got %q want /explicit/key", got)
	}
}

func TestResolveHostKey_EnvVar(t *testing.T) {
	got, err := resolveHostKey("", &envLookup{
		BUCKETVCS_SSH_HOST_KEY: "/env/ssh_key",
		HOME:                   "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/env/ssh_key" {
		t.Fatalf("got %q want /env/ssh_key", got)
	}
}

func TestResolveHostKey_XDG(t *testing.T) {
	got, err := resolveHostKey("", &envLookup{
		XDG_STATE_HOME: "/x",
		HOME:           "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/x", "bucketvcs", "ssh_host_ed25519_key")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveHostKey_HOMEFallback(t *testing.T) {
	got, err := resolveHostKey("", &envLookup{HOME: "/home/u"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/u", ".local", "state", "bucketvcs", "ssh_host_ed25519_key")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveHostKey_NoHOME(t *testing.T) {
	if _, err := resolveHostKey("", &envLookup{}); err == nil {
		t.Fatal("want error when HOME unset and no other source")
	}
}
