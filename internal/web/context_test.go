package web

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestSessionContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if SessionFromContext(ctx) != nil {
		t.Fatal("empty ctx must yield nil session")
	}
	sess := &auth.Session{UserID: "u1", Name: "alice"}
	ctx = withSession(ctx, sess)
	got := SessionFromContext(ctx)
	if got == nil || got.Name != "alice" {
		t.Fatalf("got %+v", got)
	}
}
