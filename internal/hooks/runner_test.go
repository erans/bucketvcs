package hooks_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func mkConfig(dir string) hooks.RunnerConfig {
	return hooks.RunnerConfig{
		HooksRoot:   dir,
		UseSandbox:  false, // exercise the unsafe-no-sandbox path for unit tests
		TimeoutSec:  10,
		CPUSec:      5,
		MemoryMB:    128,
		OutputMaxKB: 4,
	}
}

func TestRunner_AcceptingScript(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "ok.sh", "#!/bin/sh\necho hello-out\necho hello-err >&2\nexit 0\n")
	r := hooks.NewRunner(mkConfig(dir))
	res := r.Run(context.Background(), "", "ok.sh", []byte("ignored stdin\n"), nil)
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "hello-out") {
		t.Errorf("Stdout = %q, want contains hello-out", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "hello-err") {
		t.Errorf("Stderr = %q, want contains hello-err", res.Stderr)
	}
}

func TestRunner_RejectingScript_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "reject.sh", "#!/bin/sh\necho 'reason' >&2\nexit 7\n")
	res := hooks.NewRunner(mkConfig(dir)).Run(context.Background(), "", "reject.sh", nil, nil)
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil (non-zero exit is not a runner error)", res.Err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "reason") {
		t.Errorf("Stderr = %q", res.Stderr)
	}
}

func TestRunner_ScriptNotFound(t *testing.T) {
	dir := t.TempDir()
	res := hooks.NewRunner(mkConfig(dir)).Run(context.Background(), "", "absent.sh", nil, nil)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not found") {
		t.Errorf("Err = %v, want ErrScriptNotFound", res.Err)
	}
}

func TestRunner_StdinDelivered(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "echo-stdin.sh", "#!/bin/sh\ncat\n")
	res := hooks.NewRunner(mkConfig(dir)).Run(context.Background(), "", "echo-stdin.sh",
		[]byte("hello stdin world\n"), nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(string(res.Stdout), "hello stdin world") {
		t.Errorf("Stdout = %q, want stdin delivered", res.Stdout)
	}
}

func TestRunner_EnvVarsExposed(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "env.sh", "#!/bin/sh\necho \"$BUCKETVCS_TENANT/$BUCKETVCS_REPO\"\n")
	res := hooks.NewRunner(mkConfig(dir)).Run(context.Background(), "", "env.sh", nil,
		map[string]string{"BUCKETVCS_TENANT": "acme", "BUCKETVCS_REPO": "site"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(string(res.Stdout), "acme/site") {
		t.Errorf("Stdout = %q, want acme/site", res.Stdout)
	}
}

func TestRunner_Timeout(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "slow.sh", "#!/bin/sh\nsleep 10\n")
	cfg := mkConfig(dir)
	cfg.TimeoutSec = 1
	start := time.Now()
	res := hooks.NewRunner(cfg).Run(context.Background(), "", "slow.sh", nil, nil)
	elapsed := time.Since(start)
	if res.Err == nil {
		t.Errorf("Err = nil, want ErrTimeout")
	}
	if elapsed > 3*time.Second {
		t.Errorf("elapsed = %v, want <= 3s (timeout=1s + grace=1s)", elapsed)
	}
}

func TestRunner_OutputTruncation(t *testing.T) {
	dir := t.TempDir()
	// Emit ~8 KB to stderr; cap is 4 KB.
	writeScript(t, dir, "big.sh",
		"#!/bin/sh\n"+
			"i=0\n"+
			"while [ $i -lt 100 ]; do\n"+
			"  echo 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' >&2\n"+
			"  i=$((i+1))\n"+
			"done\n")
	res := hooks.NewRunner(mkConfig(dir)).Run(context.Background(), "", "big.sh", nil, nil)
	if len(res.Stderr) > 4*1024+64 { // cap + small "[truncated]" marker
		t.Errorf("Stderr length = %d, want <= 4 KB + marker", len(res.Stderr))
	}
}

func TestRunner_ScriptNameWithPathSeparator_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "ok.sh", "#!/bin/sh\nexit 0\n")
	res := hooks.NewRunner(mkConfig(dir)).Run(context.Background(), "", "../etc/passwd", nil, nil)
	if res.Err == nil {
		t.Errorf("Err = nil, want validation rejection for path-traversal script name")
	}
}
