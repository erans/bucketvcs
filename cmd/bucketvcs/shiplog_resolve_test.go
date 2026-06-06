package main

import (
	"path/filepath"
	"testing"
)

func TestResolveSpoolDir_FlagWins(t *testing.T) {
	got, err := resolveSpoolDir("/explicit", &envLookup{
		BUCKETVCS_LOG_SPOOL_DIR: "/env",
		XDG_STATE_HOME:          "/xdg",
		HOME:                    "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit" {
		t.Fatalf("got %q want /explicit", got)
	}
}

func TestResolveSpoolDir_EnvVar(t *testing.T) {
	got, err := resolveSpoolDir("", &envLookup{
		BUCKETVCS_LOG_SPOOL_DIR: "/envdir",
		XDG_STATE_HOME:          "/xdg",
		HOME:                    "/home/u",
	})
	if err != nil || got != "/envdir" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveSpoolDir_XDG(t *testing.T) {
	got, err := resolveSpoolDir("", &envLookup{
		XDG_STATE_HOME: "/xdg",
		HOME:           "/home/u",
	})
	want := filepath.Join("/xdg", "bucketvcs", "log-spool")
	if err != nil || got != want {
		t.Fatalf("got %q want %q err %v", got, want, err)
	}
}

func TestResolveSpoolDir_HOMEFallback(t *testing.T) {
	got, err := resolveSpoolDir("", &envLookup{HOME: "/home/u"})
	want := filepath.Join("/home/u", ".local", "state", "bucketvcs", "log-spool")
	if err != nil || got != want {
		t.Fatalf("got %q want %q err %v", got, want, err)
	}
}

func TestResolveSpoolDir_NoHOME(t *testing.T) {
	if _, err := resolveSpoolDir("", &envLookup{}); err == nil {
		t.Fatal("want error when no source resolvable")
	}
}
