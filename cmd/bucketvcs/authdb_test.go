package main

import (
	"path/filepath"
	"testing"
)

func TestResolveAuthDB_FlagWins(t *testing.T) {
	got, err := resolveAuthDB("/explicit", &envLookup{
		BUCKETVCS_AUTH_DB: "/env",
		XDG_STATE_HOME:    "/xdg",
		HOME:              "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit" {
		t.Fatalf("got %q want /explicit", got)
	}
}

func TestResolveAuthDB_EnvVar(t *testing.T) {
	got, _ := resolveAuthDB("", &envLookup{
		BUCKETVCS_AUTH_DB: "/env/path.db",
		HOME:              "/home/u",
	})
	if got != "/env/path.db" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveAuthDB_XDG(t *testing.T) {
	got, _ := resolveAuthDB("", &envLookup{
		XDG_STATE_HOME: "/x",
		HOME:           "/home/u",
	})
	want := filepath.Join("/x", "bucketvcs", "bucketvcs.db")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveAuthDB_HOMEFallback(t *testing.T) {
	got, _ := resolveAuthDB("", &envLookup{HOME: "/home/u"})
	want := filepath.Join("/home/u", ".local", "state", "bucketvcs", "bucketvcs.db")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveAuthDB_NoHOME(t *testing.T) {
	if _, err := resolveAuthDB("", &envLookup{}); err == nil {
		t.Fatal("want error when HOME unset and no other source")
	}
}
