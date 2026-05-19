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
