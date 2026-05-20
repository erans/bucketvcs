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

func TestEmitLFSSSHAuthenticate_Shape(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSSSHAuthenticate(context.Background(), captureLogger(&buf), "acme/foo", "alice", "upload", 900, "ok")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.ssh_authenticate"`,
		`"event":"lfs.ssh_authenticate"`,
		`"repo":"acme/foo"`,
		`"user":"alice"`,
		`"op":"upload"`,
		`"ttl_seconds":900`,
		`"result":"ok"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSSSHAuthenticate_AnonZeroTTL(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSSSHAuthenticate(context.Background(), captureLogger(&buf), "acme/foo", "", "download", 0, "anon")
	line := buf.String()
	for _, want := range []string{
		`"user":""`,
		`"ttl_seconds":0`,
		`"result":"anon"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSLockCreate_Shape(t *testing.T) {
	var buf bytes.Buffer
	emitLFSLockCreate(context.Background(), captureLogger(&buf), "acme/demo", "alice", "u_alice", "lock_001", "art/hero.psd", "refs/heads/main")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.lock.create"`,
		`"event":"lfs.lock.create"`,
		`"repo":"acme/demo"`,
		`"user":"alice"`,
		`"owner_user_id":"u_alice"`,
		`"lock_id":"lock_001"`,
		`"path":"art/hero.psd"`,
		`"ref_name":"refs/heads/main"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSLockDelete_Shape_Owner(t *testing.T) {
	var buf bytes.Buffer
	emitLFSLockDelete(context.Background(), captureLogger(&buf), "acme/demo", "alice", "lock_001", false, "")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.lock.delete"`,
		`"event":"lfs.lock.delete"`,
		`"repo":"acme/demo"`,
		`"user":"alice"`,
		`"lock_id":"lock_001"`,
		`"force":false`,
		`"force_target_user_id":""`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSLockDelete_Shape_Force(t *testing.T) {
	var buf bytes.Buffer
	emitLFSLockDelete(context.Background(), captureLogger(&buf), "acme/demo", "bob", "lock_001", true, "u_alice")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.lock.delete"`,
		`"event":"lfs.lock.delete"`,
		`"repo":"acme/demo"`,
		`"user":"bob"`,
		`"lock_id":"lock_001"`,
		`"force":true`,
		`"force_target_user_id":"u_alice"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSLockVerify_Shape(t *testing.T) {
	var buf bytes.Buffer
	emitLFSLockVerify(context.Background(), captureLogger(&buf), "acme/demo", "alice", 3, 2)
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.lock.verify"`,
		`"event":"lfs.lock.verify"`,
		`"repo":"acme/demo"`,
		`"user":"alice"`,
		`"ours_count":3`,
		`"theirs_count":2`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}
