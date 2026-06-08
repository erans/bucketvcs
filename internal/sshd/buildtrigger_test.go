package sshd

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

// passThroughVerify is a no-op auth.Store verify hook for plumbing tests that
// never actually authenticate a client.
func passThroughVerify(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
	return &auth.Actor{UserID: "u1", Name: "alice"}, "k1", nil, nil
}

// TestOptions_BuildTriggersPlumbed confirms the M30 build-trigger Service set
// on sshd.Options is retained on the constructed Server's opts, so the SSH
// receive-pack session handler (session.go) can forward it into the
// receivepack.EngineRequest as BuildTriggers. This closes the SSH-side wiring
// gap that mirrored the gateway wiring in Task 12.
//
// The EngineRequest itself is built inside the live session handler, which is
// only reachable through a real SSH push; that end-to-end flow is covered by
// e2e_test.go and the Task 14 smoke. This unit test asserts the value survives
// Options -> Server.opts, which is the link that was missing.
func TestOptions_BuildTriggersPlumbed(t *testing.T) {
	// A non-nil Service is enough for the plumbing assertion; we never call
	// its methods, so a nil db handle is fine (New does not touch it).
	svc := buildtrigger.New(nil)

	opts := newTestServerOpts(t, &fakeStore{verify: passThroughVerify})
	opts.BuildTriggers = svc

	s, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.opts.BuildTriggers != svc {
		t.Fatalf("Server.opts.BuildTriggers = %v, want the Service passed via Options", s.opts.BuildTriggers)
	}

	// And the pre-M30 default: a Server built without BuildTriggers carries nil,
	// so the EngineRequest's `if eng.BuildTriggers != nil` guard stays a no-op.
	plain, err := NewServer(newTestServerOpts(t, &fakeStore{verify: passThroughVerify}))
	if err != nil {
		t.Fatalf("NewServer (default): %v", err)
	}
	if plain.opts.BuildTriggers != nil {
		t.Fatalf("default Server.opts.BuildTriggers = %v, want nil", plain.opts.BuildTriggers)
	}
}
