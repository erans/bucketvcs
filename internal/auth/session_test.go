package auth

import "testing"

func TestSessionZeroValue(t *testing.T) {
	var s Session
	if s.UserID != "" || s.Name != "" || s.IsAdmin {
		t.Fatalf("zero Session not empty: %+v", s)
	}
	if ErrNoSession == nil {
		t.Fatal("ErrNoSession must be a non-nil sentinel")
	}
}
