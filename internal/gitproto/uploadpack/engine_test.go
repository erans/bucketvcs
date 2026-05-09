package uploadpack

import "testing"

func TestStubsCompile(t *testing.T) {
	if err := Advertise(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Advertise stub")
	}
	if err := Service(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Service stub")
	}
	// Serve calls Advertise; same expectation.
	if err := Serve(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Serve")
	}
}
