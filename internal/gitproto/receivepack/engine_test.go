package receivepack

import "testing"

func TestStubsCompile(t *testing.T) {
	if err := Advertise(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Advertise stub")
	}
	if err := Service(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Service stub")
	}
	if err := Serve(&EngineRequest{}); err == nil {
		t.Fatal("expected ErrNotImplemented from Serve")
	}
}
