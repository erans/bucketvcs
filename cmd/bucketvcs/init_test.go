package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunInit_HappyPath(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInit(context.Background(), []string{
		"--store=localfs:" + dir,
		"--actor=u_test",
		"acme", "my-repo",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created acme/my-repo") {
		t.Errorf("missing success line in stdout: %q", stdout.String())
	}
}

func TestRunInit_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--store=localfs:" + dir, "acme", "my-repo"}

	var sink bytes.Buffer
	if code := runInit(context.Background(), args, &sink, &sink); code != 0 {
		t.Fatalf("first init failed: exit %d", code)
	}
	var stdout, stderr bytes.Buffer
	code := runInit(context.Background(), args, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on duplicate init")
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr should mention 'already exists', got %q", stderr.String())
	}
}

func TestRunInit_BadFlags(t *testing.T) {
	cases := [][]string{
		{}, // missing positional + missing --store
		{"--store=localfs:/tmp/x"},                  // missing positional
		{"--store=", "a", "b"},                      // empty store
		{"--store=localfs:/tmp/x", "a"},             // missing repo
		{"--store=localfs:/tmp/x", "a", "b", "c"},   // too many
	}
	for i, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := runInit(context.Background(), c, &stdout, &stderr); code == 0 {
			t.Errorf("case %d: expected non-zero exit, got 0; stderr=%s", i, stderr.String())
		}
	}
}
