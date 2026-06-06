package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// assertAuditShape decodes one JSON log line and asserts the shiplog tap
// contract: audit==true and event==msg. Every genuine lfs.* audit emitter
// must satisfy this so it lands in the durable activity stream.
func assertAuditShape(t *testing.T, line string) {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &rec); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, line)
	}
	if a, ok := rec["audit"].(bool); !ok || !a {
		t.Errorf("audit attr missing or not true: %s", line)
	}
	if rec["event"] != rec["msg"] {
		t.Errorf("event (%v) != msg (%v): %s", rec["event"], rec["msg"], line)
	}
}

// TestLFSAuditEmitters_AuditShape covers every lfs audit emitter and asserts
// audit=true + event==msg. Keeps the taxonomy from drifting out of the
// activity stream.
func TestLFSAuditEmitters_AuditShape(t *testing.T) {
	ctx := context.Background()
	oid := strings.Repeat("a", 64)
	type tc struct {
		name  string
		event string
		run   func(*bytes.Buffer)
	}
	tcs := []tc{
		{"batch", "lfs.batch", func(b *bytes.Buffer) { emitLFSBatch(ctx, captureLogger(b), "a/r", "u", "upload", 1, "ok") }},
		{"object_served", "lfs.object.served", func(b *bytes.Buffer) { emitLFSObjectServed(ctx, captureLogger(b), "upload", "a/r/o", 1, 200) }},
		{"verify", "lfs.verify", func(b *bytes.Buffer) { emitLFSVerify(ctx, captureLogger(b), "a/r", "u", oid, 1, "ok") }},
		{"lock_create", "lfs.lock.create", func(b *bytes.Buffer) {
			emitLFSLockCreate(ctx, captureLogger(b), "a/r", "u", "uid", "lid", "p", "refs/heads/main")
		}},
		{"lock_delete", "lfs.lock.delete", func(b *bytes.Buffer) { emitLFSLockDelete(ctx, captureLogger(b), "a/r", "u", "lid", false, "") }},
		{"lock_verify", "lfs.lock.verify", func(b *bytes.Buffer) { emitLFSLockVerify(ctx, captureLogger(b), "a/r", "u", 1, 0) }},
		{"ssh_authenticate", "lfs.ssh_authenticate", func(b *bytes.Buffer) { EmitLFSSSHAuthenticate(ctx, captureLogger(b), "a/r", "u", "upload", 900, "ok") }},
		{"gc_mark", "lfs.gc.mark", func(b *bytes.Buffer) { EmitLFSGCMark(ctx, captureLogger(b), "a/r", "m", 1, 1, false) }},
		{"gc_sweep", "lfs.gc.sweep", func(b *bytes.Buffer) { EmitLFSGCSweep(ctx, captureLogger(b), "a/r", "m", "s", 1, 0, 0, 0, 1, false) }},
		{"quota_exceeded", "lfs.quota.exceeded", func(b *bytes.Buffer) { EmitLFSQuotaExceeded(ctx, captureLogger(b), "a", 1, 2, 1, "o") }},
		{"quota_reconcile", "lfs.quota.reconcile", func(b *bytes.Buffer) { EmitLFSQuotaReconcile(ctx, captureLogger(b), "a", 1, 1, 0, false) }},
	}
	for _, c := range tcs {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			c.run(&buf)
			if !strings.Contains(buf.String(), c.event) {
				t.Errorf("event %q missing: %s", c.event, buf.String())
			}
			assertAuditShape(t, buf.String())
		})
	}
}

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

func TestEmitLFSGCMark_Shape(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSGCMark(context.Background(), captureLogger(&buf), "acme/repo", "lfs-20260520T120000Z-deadbeef", 17, 42, false)
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.gc.mark"`,
		`"event":"lfs.gc.mark"`,
		`"repo":"acme/repo"`,
		`"mark_id":"lfs-20260520T120000Z-deadbeef"`,
		`"candidates_count":17`,
		`"manifest_version":42`,
		`"dry_run":false`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSGCMark_DryRun(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSGCMark(context.Background(), captureLogger(&buf), "acme/repo", "m", 0, 1, true)
	if !strings.Contains(buf.String(), `"dry_run":true`) {
		t.Errorf("expected dry_run=true in: %s", buf.String())
	}
}

func TestEmitLFSGCSweep_Shape(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSGCSweep(context.Background(), captureLogger(&buf), "acme/repo",
		"lfs-20260520T120000Z-deadbeef", "lfs-sweep-20260520T120100Z-cafebabe",
		7, 3, 1, 0, 8192, false)
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.gc.sweep"`,
		`"event":"lfs.gc.sweep"`,
		`"repo":"acme/repo"`,
		`"mark_id":"lfs-20260520T120000Z-deadbeef"`,
		`"sweep_id":"lfs-sweep-20260520T120100Z-cafebabe"`,
		`"deleted_count":7`,
		`"deleted_bytes":8192`,
		`"skipped_retention":3`,
		`"skipped_concurrent":1`,
		`"error_count":0`,
		`"dry_run":false`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSGCSweep_DryRun(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSGCSweep(context.Background(), captureLogger(&buf), "acme/repo", "m", "s", 0, 0, 0, 0, 0, true)
	if !strings.Contains(buf.String(), `"dry_run":true`) {
		t.Errorf("expected dry_run=true in: %s", buf.String())
	}
}

func TestEmitLFSQuotaExceeded_Shape(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSQuotaExceeded(context.Background(), captureLogger(&buf),
		"acme", 99<<30, 100<<30, 10<<30, "oid1,oid2")
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.quota.exceeded"`,
		`"event":"lfs.quota.exceeded"`,
		`"tenant":"acme"`,
		`"current_bytes":106300440576`,
		`"limit_bytes":107374182400`,
		`"requested_bytes":10737418240`,
		`"oids":"oid1,oid2"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitLFSQuotaReconcile_Shape(t *testing.T) {
	var buf bytes.Buffer
	EmitLFSQuotaReconcile(context.Background(), captureLogger(&buf),
		"acme", 500, 487, -13, false)
	line := buf.String()
	for _, want := range []string{
		`"msg":"lfs.quota.reconcile"`,
		`"event":"lfs.quota.reconcile"`,
		`"tenant":"acme"`,
		`"before_bytes":500`,
		`"after_bytes":487`,
		`"drift_bytes":-13`,
		`"dry_run":false`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}
