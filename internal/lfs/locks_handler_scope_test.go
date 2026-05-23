// Round-2 M17 roborev fix M1: dedicated tests covering the LFS Locks
// handlers' M17 token-scope enforcement. handleLocksCreate /
// handleLocksUnlock require lfs:write; handleLocksList /
// handleLocksVerify require lfs:read. Legacy tokens bypass.
package lfs_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
)

// scopedActor returns an actor for the named user with the given scope
// bitmask. Used to drive non-legacy actors through the locks handler.
func scopedActor(t *testing.T, h *locksTestHarness, name string, scopes auth.TokenScope) *auth.Actor {
	t.Helper()
	a := h.createUser(t, name)
	a.Scopes = scopes
	return a
}

// --- handleLocksCreate: requires lfs:write ---

func TestLocksCreate_ForbiddenWithoutLFSWrite(t *testing.T) {
	h := newLocksHarness(t)
	// lfs:read is the relevant boundary — caller can list/verify locks
	// but must NOT be able to create one.
	h.actor = scopedActor(t, h, "alice", auth.ScopeLFSRead)

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{Path: "art/hero.psd"})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s want 403", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lfs:write") {
		t.Errorf("body should mention lfs:write scope: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient scope") {
		t.Errorf("body should mention 'insufficient scope': %s", w.Body.String())
	}
}

func TestLocksCreate_AllowedWithLFSWrite(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = scopedActor(t, h, "alice", auth.ScopeLFSWrite)

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{Path: "art/hero.psd"})

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s want 201", w.Code, w.Body.String())
	}
	var resp lfs.LockResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.Lock.Path != "art/hero.psd" {
		t.Errorf("Path=%q", resp.Lock.Path)
	}
}

func TestLocksCreate_LegacyTokenBypasses(t *testing.T) {
	h := newLocksHarness(t)
	// Default actor returned by createUser has Scopes==ScopeLegacy.
	h.actor = h.createUser(t, "alice")
	if h.actor.Scopes != auth.ScopeLegacy {
		t.Fatalf("test setup: expected Scopes==ScopeLegacy on default actor, got %v", h.actor.Scopes)
	}

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{Path: "art/hero.psd"})

	// Legacy bypass: scope check is skipped. Expect 201 success.
	if w.Code != http.StatusCreated {
		t.Fatalf("legacy actor unexpectedly rejected: status=%d body=%s", w.Code, w.Body.String())
	}
}

// --- handleLocksList: requires lfs:read ---

func TestLocksList_ForbiddenWithoutLFSRead(t *testing.T) {
	h := newLocksHarness(t)
	// repo:read is unrelated to LFS — the actor must be rejected.
	h.actor = scopedActor(t, h, "alice", auth.ScopeRepoRead)

	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks", nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s want 403", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lfs:read") {
		t.Errorf("body should mention lfs:read scope: %s", w.Body.String())
	}
}

func TestLocksList_AllowedWithLFSRead(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = scopedActor(t, h, "alice", auth.ScopeLFSRead)

	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", w.Code, w.Body.String())
	}
}

// --- handleLocksVerify: requires lfs:read ---

func TestLocksVerify_ForbiddenWithoutLFSRead(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = scopedActor(t, h, "alice", auth.ScopeRepoRead)

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/verify", nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s want 403", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lfs:read") {
		t.Errorf("body should mention lfs:read scope: %s", w.Body.String())
	}
}

func TestLocksVerify_AllowedWithLFSRead(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = scopedActor(t, h, "alice", auth.ScopeLFSRead)

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/verify", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", w.Code, w.Body.String())
	}
}

// --- handleLocksUnlock: requires lfs:write ---

func TestLocksUnlock_ForbiddenWithoutLFSWrite(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = scopedActor(t, h, "alice", auth.ScopeLFSRead)

	// We don't need a real lock ID — the scope check fires before the
	// store lookup.
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/lock_doesnotexist/unlock",
		map[string]any{})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s want 403", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lfs:write") {
		t.Errorf("body should mention lfs:write scope: %s", w.Body.String())
	}
}

func TestLocksUnlock_AllowedWithLFSWrite(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = scopedActor(t, h, "alice", auth.ScopeLFSWrite)

	// Unlock against a non-existent lock: the scope check passes, so
	// the handler reaches the store lookup which 404s. We assert the
	// scope path let us through (not 403).
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/lock_doesnotexist/unlock",
		map[string]any{})

	if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "insufficient scope") {
		t.Fatalf("scope check unexpectedly rejected lfs:write actor: status=%d body=%s",
			w.Code, w.Body.String())
	}
	// We also assert the expected downstream status to ensure the
	// request reached the lookup path, not some other deny.
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s want 404 (lock not found)", w.Code, w.Body.String())
	}
}
