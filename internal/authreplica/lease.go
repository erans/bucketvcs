package authreplica

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrLeaseHeld is returned by Acquire when another live instance holds the lease.
var ErrLeaseHeld = errors.New("authreplica: lease held by another instance")

// ErrLeaseLost is returned by Renew when the lease was taken over.
var ErrLeaseLost = errors.New("authreplica: lease lost to another instance")

// acquireAttempts bounds the PutIfAbsent/Get retry loop in Acquire. Each
// retry corresponds to the holder releasing the lease in the narrow window
// between our failed PutIfAbsent and our Get. A handful of attempts is far
// more than any non-pathological race needs; the bound guarantees Acquire
// always terminates rather than recursing/looping forever under contention.
const acquireAttempts = 4

// leaseDoc is the JSON body of <prefix>/lease.json.
type leaseDoc struct {
	InstanceID string    `json:"instance_id"`
	Hostname   string    `json:"hostname"`
	PID        int       `json:"pid"`
	RenewedAt  time.Time `json:"renewed_at"`
	TTLSeconds int64     `json:"ttl_s"`
}

// Lease is a CAS lease over a single object. It protects the replica
// lineage from concurrent writers (split-brain), not the local DB.
type Lease struct {
	store storage.ObjectStore
	key   string
	ttl   time.Duration
	now   func() time.Time // test seam

	id  string
	ver storage.ObjectVersion

	// tookOver records that Acquire succeeded by expiring a previous
	// holder's lease (vs a fresh claim). Read via TookOver for auditing.
	tookOver   bool
	prevHolder string
}

// NewLease returns an unacquired lease at <prefix>/lease.json.
func NewLease(store storage.ObjectStore, prefix string, ttl time.Duration) *Lease {
	idb := make([]byte, 16)
	_, _ = rand.Read(idb)
	return &Lease{
		store: store,
		key:   path.Join(prefix, "lease.json"),
		ttl:   ttl,
		now:   time.Now,
		id:    hex.EncodeToString(idb),
	}
}

// InstanceID returns this instance's random identity.
func (l *Lease) InstanceID() string { return l.id }

// TookOver reports whether the last successful Acquire expired a previous
// holder's lease, and that holder's instance id.
func (l *Lease) TookOver() (bool, string) { return l.tookOver, l.prevHolder }

func (l *Lease) body() ([]byte, error) {
	host, _ := os.Hostname()
	return json.Marshal(leaseDoc{
		InstanceID: l.id,
		Hostname:   host,
		PID:        os.Getpid(),
		RenewedAt:  l.now().UTC(),
		TTLSeconds: int64(l.ttl / time.Second),
	})
}

// Acquire claims the lease: PutIfAbsent, or CAS takeover if the current
// holder's lease has expired. A live holder yields ErrLeaseHeld naming it.
func (l *Lease) Acquire(ctx context.Context) error {
	for attempt := 0; attempt < acquireAttempts; attempt++ {
		b, err := l.body()
		if err != nil {
			return err
		}
		ver, err := l.store.PutIfAbsent(ctx, l.key, bytes.NewReader(b), nil)
		if err == nil {
			l.ver = ver
			return nil
		}
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return fmt.Errorf("authreplica: acquire lease: %w", err)
		}

		obj, err := l.store.Get(ctx, l.key, nil)
		if errors.Is(err, storage.ErrNotFound) {
			// Released between PutIfAbsent and Get; retry the create path.
			continue
		}
		if err != nil {
			return fmt.Errorf("authreplica: read lease: %w", err)
		}
		raw, readErr := io.ReadAll(obj.Body)
		obj.Body.Close()
		if readErr != nil {
			return fmt.Errorf("authreplica: read lease: %w", readErr)
		}
		var doc leaseDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("authreplica: parse lease %s: %w", l.key, err)
		}

		expiry := doc.RenewedAt.Add(time.Duration(doc.TTLSeconds) * time.Second)
		if l.now().Before(expiry) {
			return fmt.Errorf("%w: instance=%s host=%s pid=%d renewed_at=%s",
				ErrLeaseHeld, doc.InstanceID, doc.Hostname, doc.PID, doc.RenewedAt.Format(time.RFC3339))
		}

		// Expired — take over via CAS on the stale version.
		ver, err = l.store.PutIfVersionMatches(ctx, l.key, obj.Metadata.Version, bytes.NewReader(b), nil)
		if err != nil {
			if errors.Is(err, storage.ErrVersionMismatch) || errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("%w: lost takeover race", ErrLeaseHeld)
			}
			return fmt.Errorf("authreplica: lease takeover: %w", err)
		}
		l.ver = ver
		l.tookOver = true
		l.prevHolder = doc.InstanceID
		return nil
	}
	return fmt.Errorf("%w: contention exhausted %d acquire attempts", ErrLeaseHeld, acquireAttempts)
}

// Renew refreshes RenewedAt via CAS on our held version. ErrLeaseLost means
// another instance took over — the caller must stop replicating.
func (l *Lease) Renew(ctx context.Context) error {
	b, err := l.body()
	if err != nil {
		return err
	}
	ver, err := l.store.PutIfVersionMatches(ctx, l.key, l.ver, bytes.NewReader(b), nil)
	if err == nil {
		l.ver = ver
		return nil
	}
	if errors.Is(err, storage.ErrVersionMismatch) || errors.Is(err, storage.ErrNotFound) {
		return ErrLeaseLost
	}
	return fmt.Errorf("authreplica: renew lease: %w", err)
}

// Release deletes the lease if we still hold it; losing it is not an error.
func (l *Lease) Release(ctx context.Context) error {
	err := l.store.DeleteIfVersionMatches(ctx, l.key, l.ver)
	if err == nil || errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrVersionMismatch) {
		return nil
	}
	return fmt.Errorf("authreplica: release lease: %w", err)
}
