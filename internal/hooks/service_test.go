package hooks_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

func newTestServiceWithStore(t *testing.T) (*hooks.Service, *hooks.Store, string) {
	t.Helper()
	authS, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authS.Close() })
	if err := authS.RegisterRepo(context.Background(), "acme", "site"); err != nil {
		t.Fatal(err)
	}
	store := hooks.NewStore(authS.DB())
	hooksRoot := t.TempDir()
	svc := hooks.NewService(store, hooks.RunnerConfig{
		HooksRoot: hooksRoot, UseSandbox: false,
		TimeoutSec: 10, CPUSec: 5, MemoryMB: 128, OutputMaxKB: 4,
	}, hooks.ServiceConfig{
		PostReceiveConcurrency: 2,
		PostReceiveQueueSize:   8,
		OnInternalError:        hooks.InternalErrorReject,
	})
	return svc, store, hooksRoot
}

func writeHookScript(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestService_RunPreReceive_NoHooks_NoOp(t *testing.T) {
	svc, _, _ := newTestServiceWithStore(t)
	defer svc.Close()
	err := svc.RunPreReceive(context.Background(), hooks.PreReceivePayload{
		Tenant: "acme", Repo: "site",
		Updates: []hooks.RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
	})
	if err != nil {
		t.Errorf("RunPreReceive with zero hooks: %v, want nil", err)
	}
}

func TestService_RunPreReceive_AcceptingHookPasses(t *testing.T) {
	svc, store, root := newTestServiceWithStore(t)
	defer svc.Close()
	writeHookScript(t, root, "ok.sh", "#!/bin/sh\nexit 0\n")
	if err := store.Add(context.Background(), hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPreReceive,
		ScriptName: "ok.sh", Enabled: true, Now: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	err := svc.RunPreReceive(context.Background(), hooks.PreReceivePayload{
		Tenant: "acme", Repo: "site",
		Updates: []hooks.RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
	})
	if err != nil {
		t.Errorf("RunPreReceive: %v, want nil", err)
	}
}

func TestService_RunPreReceive_RejectingHookReturnsHookRejection(t *testing.T) {
	svc, store, root := newTestServiceWithStore(t)
	defer svc.Close()
	writeHookScript(t, root, "reject.sh", "#!/bin/sh\necho 'no good' >&2\nexit 5\n")
	_ = store.Add(context.Background(), hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPreReceive,
		ScriptName: "reject.sh", Enabled: true, Now: time.Unix(1, 0),
	})
	err := svc.RunPreReceive(context.Background(), hooks.PreReceivePayload{
		Tenant: "acme", Repo: "site",
		Updates: []hooks.RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
	})
	if err == nil {
		t.Fatal("expected HookRejection, got nil")
	}
	var hr *hooks.HookRejection
	if !errors.As(err, &hr) {
		t.Fatalf("err = %v, want HookRejection", err)
	}
	if hr.ExitCode != 5 {
		t.Errorf("ExitCode = %d, want 5", hr.ExitCode)
	}
	if hr.ScriptName != "reject.sh" {
		t.Errorf("ScriptName = %s, want reject.sh", hr.ScriptName)
	}
}

func TestService_RunPreReceive_FailFastOnFirstReject(t *testing.T) {
	svc, store, root := newTestServiceWithStore(t)
	defer svc.Close()
	writeHookScript(t, root, "a.sh", "#!/bin/sh\nexit 0\n")
	writeHookScript(t, root, "b.sh", "#!/bin/sh\nexit 1\n")
	// c.sh deliberately touches a marker if it runs; this proves fail-fast.
	writeHookScript(t, root, "c.sh", "#!/bin/sh\ntouch "+filepath.Join(root, "c-ran")+"\nexit 0\n")
	for i, name := range []string{"a.sh", "b.sh", "c.sh"} {
		_ = store.Add(context.Background(), hooks.Row{
			Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPreReceive,
			ScriptName: name, SortOrder: i * 10, Enabled: true, Now: time.Unix(int64(i), 0),
		})
	}
	err := svc.RunPreReceive(context.Background(), hooks.PreReceivePayload{
		Tenant: "acme", Repo: "site",
		Updates: []hooks.RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
	})
	if err == nil {
		t.Fatal("expected HookRejection")
	}
	if _, statErr := os.Stat(filepath.Join(root, "c-ran")); statErr == nil {
		t.Errorf("c.sh ran after b.sh rejection — fail-fast broken")
	}
}

func TestService_RunPreReceive_ScriptMissing_RejectByDefault(t *testing.T) {
	svc, store, _ := newTestServiceWithStore(t)
	defer svc.Close()
	_ = store.Add(context.Background(), hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPreReceive,
		ScriptName: "ghost.sh", Enabled: true, Now: time.Unix(1, 0),
	})
	err := svc.RunPreReceive(context.Background(), hooks.PreReceivePayload{
		Tenant: "acme", Repo: "site",
		Updates: []hooks.RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
	})
	if err == nil {
		t.Error("expected internal error rejection (default fail-closed)")
	}
}

func TestService_RunPreReceive_InternalErrorAllowMode_ProceedsOnMissingScript(t *testing.T) {
	authS, _ := sqlitestore.Open(":memory:")
	defer authS.Close()
	_ = authS.RegisterRepo(context.Background(), "acme", "site")
	store := hooks.NewStore(authS.DB())
	root := t.TempDir()
	svc := hooks.NewService(store, hooks.RunnerConfig{
		HooksRoot: root, UseSandbox: false,
		TimeoutSec: 10, CPUSec: 5, MemoryMB: 128, OutputMaxKB: 4,
	}, hooks.ServiceConfig{
		PostReceiveConcurrency: 2,
		PostReceiveQueueSize:   8,
		OnInternalError:        hooks.InternalErrorAllow,
	})
	defer svc.Close()
	_ = store.Add(context.Background(), hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPreReceive,
		ScriptName: "ghost.sh", Enabled: true, Now: time.Unix(1, 0),
	})
	err := svc.RunPreReceive(context.Background(), hooks.PreReceivePayload{
		Tenant: "acme", Repo: "site",
		Updates: []hooks.RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
	})
	if err != nil {
		t.Errorf("with InternalErrorAllow + missing script: err=%v, want nil", err)
	}
}

func TestService_EnqueuePostReceive_RunsScript(t *testing.T) {
	svc, store, root := newTestServiceWithStore(t)
	defer svc.Close()
	marker := filepath.Join(root, "post-ran")
	writeHookScript(t, root, "log.sh", "#!/bin/sh\ntouch "+marker+"\n")
	_ = store.Add(context.Background(), hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPostReceive,
		ScriptName: "log.sh", Enabled: true, Now: time.Unix(1, 0),
	})
	svc.EnqueuePostReceive(hooks.PostReceivePayload{
		PreReceivePayload: hooks.PreReceivePayload{Tenant: "acme", Repo: "site"},
		TxID:              "tx-1",
	})
	// Wait up to 2 seconds for the worker to run.
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("post-receive marker never appeared; worker didn't run")
}

func TestService_EnqueuePostReceive_FullQueueDrops(t *testing.T) {
	authS, _ := sqlitestore.Open(":memory:")
	defer authS.Close()
	_ = authS.RegisterRepo(context.Background(), "acme", "site")
	store := hooks.NewStore(authS.DB())
	root := t.TempDir()
	writeHookScript(t, root, "slow.sh", "#!/bin/sh\nsleep 5\n")
	_ = store.Add(context.Background(), hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: hooks.TriggerPostReceive,
		ScriptName: "slow.sh", Enabled: true, Now: time.Unix(1, 0),
	})
	svc := hooks.NewService(store, hooks.RunnerConfig{
		HooksRoot: root, UseSandbox: false,
		TimeoutSec: 10, CPUSec: 5, MemoryMB: 128, OutputMaxKB: 4,
	}, hooks.ServiceConfig{
		PostReceiveConcurrency: 1,
		PostReceiveQueueSize:   1, // tiny — easy to fill
		OnInternalError:        hooks.InternalErrorReject,
	})
	defer svc.Close()
	// Fill the worker + the channel:
	for i := 0; i < 10; i++ {
		svc.EnqueuePostReceive(hooks.PostReceivePayload{
			PreReceivePayload: hooks.PreReceivePayload{Tenant: "acme", Repo: "site"},
		})
	}
	// We don't assert exact drop count — only that the call returns
	// without blocking (proves non-blocking enqueue).
}
