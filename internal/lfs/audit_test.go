package lfs

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestEmitLFSBatch_Shape(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)
	emitLFSBatch(context.Background(), logger, "acme/foo", "alice", "upload", 3, "ok")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.batch"`,
		`"event":"lfs.batch"`,
		`"repo":"acme/foo"`,
		`"user":"alice"`,
		`"op":"upload"`,
		`"n_objects":3`,
		`"result":"ok"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSBatch_AnonymousUser(t *testing.T) {
	var buf bytes.Buffer
	emitLFSBatch(context.Background(), captureLogger(&buf), "acme/foo", "", "download", 1, "ok")
	if !strings.Contains(buf.String(), `"user":""`) {
		t.Errorf("expected empty user for anonymous; got %s", buf.String())
	}
}

func TestEmitLFSObjectServed_Shape(t *testing.T) {
	var buf bytes.Buffer
	emitLFSObjectServed(context.Background(), captureLogger(&buf), "upload", "acme/foo/oid", 1024, 200)
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.object.served"`,
		`"event":"lfs.object.served"`,
		`"op":"upload"`,
		`"hash":"acme/foo/oid"`,
		`"bytes":1024`,
		`"status":200`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSVerify_Shape(t *testing.T) {
	var buf bytes.Buffer
	oid := strings.Repeat("a", 64)
	emitLFSVerify(context.Background(), captureLogger(&buf), "acme/foo", "alice", oid, 1024, "ok")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.verify"`,
		`"event":"lfs.verify"`,
		`"repo":"acme/foo"`,
		`"user":"alice"`,
		`"oid":"` + oid + `"`,
		`"size":1024`,
		`"result":"ok"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}
