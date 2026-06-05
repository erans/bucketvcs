package authreplica

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLease_AcquireFresh(t *testing.T) {
	l := NewLease(newLocalFS(t), "sys/authdb", time.Minute)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.InstanceID() == "" {
		t.Fatal("empty instance id")
	}
}

func TestLease_SecondAcquireFailsWhileLive(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := NewLease(store, "sys/authdb", time.Minute)
	err := b.Acquire(context.Background())
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("want ErrLeaseHeld, got %v", err)
	}
	// Holder must be named for the operator.
	if !strings.Contains(err.Error(), a.InstanceID()) {
		t.Fatalf("error does not name holder: %v", err)
	}
}

func TestLease_TakeoverAfterExpiry(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := NewLease(store, "sys/authdb", time.Minute)
	b.now = func() time.Time { return time.Now().Add(2 * time.Minute) } // a's lease expired
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatalf("takeover failed: %v", err)
	}
}

func TestLease_RenewAndLoss(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}

	// b steals after expiry; a's next renew must report loss.
	b := NewLease(store, "sys/authdb", time.Minute)
	b.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Renew(context.Background()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
}

func TestLease_ReleaseAllowsReacquire(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := NewLease(store, "sys/authdb", time.Minute)
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatalf("reacquire after release failed: %v", err)
	}
}
