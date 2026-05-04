package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("want non-zero exit when no subcommand")
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr should print usage; got %q", stderr.String())
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"frobulate"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("want non-zero exit on unknown subcommand")
	}
}

func TestRun_HelpFlag(t *testing.T) {
	for _, helpArg := range []string{"-h", "--help", "help"} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{helpArg}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%q: want exit 0, got %d", helpArg, code)
		}
		if !strings.Contains(stdout.String(), "Usage:") {
			t.Errorf("%q: stdout should print usage; got %q", helpArg, stdout.String())
		}
	}
}

func TestRun_DispatchInit(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"init", "--store=localfs:" + dir, "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Errorf("init dispatch: want 0, got %d; stderr=%s", code, stderr.String())
	}
}

func TestRun_DispatchInspect(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(), []string{"init", "--store=localfs:" + dir, "acme", "x"}, &sink, &sink); code != 0 {
		t.Fatalf("init failed: %s", sink.String())
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"inspect-manifest", "--store=localfs:" + dir, "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Errorf("inspect-manifest dispatch: want 0, got %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "manifest_version") {
		t.Errorf("expected inspect output, got %q", stdout.String())
	}
}
