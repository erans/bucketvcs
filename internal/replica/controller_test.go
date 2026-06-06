package replica_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

const rootKey = "tenants/acme/repos/web/manifest/root.json"

// rootJSON templates a real root manifest captured from `bucketvcs init`,
// templating only manifest_version and latest_tx.
func rootJSON(version uint64) string {
	return fmt.Sprintf(`{"bundles":[],"created_at":"2026-06-05T01:24:35Z","default_branch":"refs/heads/main","indexes":{},"latest_tx":"tx-%[1]d","manifest_version":%[1]d,"min_reader_version":"0.1.0","packs":[],"refs":{},"repo_format":{"object_format":"sha1","compatibility":["sha1"]},"repo_id":"web","schema_version":2,"updated_at":"2026-06-05T01:24:35Z"}`, version)
}

type env struct {
	regional, canonical storage.ObjectStore
	now                 time.Time
	ctl                 *replica.Controller
}

func setup(t *testing.T, mode replica.Mode, budget, interval time.Duration) *env {
	t.Helper()
	r, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{regional: r, canonical: c, now: time.Unix(1_700_000_000, 0)}
	e.ctl = replica.NewController(replica.ControllerConfig{
		Mode:          mode,
		LagBudget:     budget,
		CheckInterval: interval,
		Regional:      r,
		Canonical:     c,
		Now:           func() time.Time { return e.now },
	})
	return e
}

func (e *env) setRoot(t *testing.T, s storage.ObjectStore, version uint64) {
	t.Helper()
	ctx := context.Background()
	body := strings.NewReader(rootJSON(version))
	if md, err := s.Head(ctx, rootKey); err == nil {
		if _, err := s.PutIfVersionMatches(ctx, rootKey, md.Version, body, nil); err != nil {
			t.Fatalf("update root: %v", err)
		}
		return
	}
	if _, err := s.PutIfAbsent(ctx, rootKey, body, nil); err != nil {
		t.Fatalf("seed root: %v", err)
	}
}

func TestBoundedStaleHealthyWhenCurrent(t *testing.T) {
	e := setup(t, replica.ModeBoundedStale, 5*time.Minute, 15*time.Second)
	e.setRoot(t, e.canonical, 7)
	e.setRoot(t, e.regional, 7)
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("current replica must be healthy: %v", err)
	}
}

func TestBoundedStaleLagWithinBudgetStillHealthy(t *testing.T) {
	e := setup(t, replica.ModeBoundedStale, 5*time.Minute, 15*time.Second)
	e.setRoot(t, e.canonical, 8)
	e.setRoot(t, e.regional, 7)
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("lag within budget must be healthy: %v", err)
	}
	e.now = e.now.Add(4 * time.Minute)
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("lag 4m within 5m budget must be healthy: %v", err)
	}
}

func TestBoundedStaleUnhealthyPastBudgetAndRecovers(t *testing.T) {
	e := setup(t, replica.ModeBoundedStale, 5*time.Minute, 15*time.Second)
	e.setRoot(t, e.canonical, 8)
	e.setRoot(t, e.regional, 7)
	_ = e.ctl.CheckAdvertise(context.Background(), "acme", "web") // begin lag tracking

	e.now = e.now.Add(6 * time.Minute)
	err := e.ctl.CheckAdvertise(context.Background(), "acme", "web")
	var uh *replica.UnhealthyError
	if !errors.As(err, &uh) {
		t.Fatalf("want UnhealthyError past budget, got %v", err)
	}

	e.setRoot(t, e.regional, 8)
	e.now = e.now.Add(16 * time.Second) // past the check TTL
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("caught-up replica must recover: %v", err)
	}
}

func TestBoundedStaleCanonicalUnreachableCountdown(t *testing.T) {
	e := setup(t, replica.ModeBoundedStale, 5*time.Minute, 15*time.Second)
	e.setRoot(t, e.canonical, 7)
	e.setRoot(t, e.regional, 7)
	_ = e.ctl.CheckAdvertise(context.Background(), "acme", "web") // successful baseline

	e.ctl.SetCanonicalForTest(failingStore{})

	e.now = e.now.Add(2 * time.Minute)
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("within budget of last successful check must serve: %v", err)
	}
	e.now = e.now.Add(4 * time.Minute) // 6m since last good check > 5m budget
	var uh *replica.UnhealthyError
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); !errors.As(err, &uh) {
		t.Fatalf("cannot-determine-lag past budget must be unhealthy, got %v", err)
	}
}

func TestStrongCurrentNeverBlocks(t *testing.T) {
	e := setup(t, replica.ModeStrongCurrent, 5*time.Minute, 15*time.Second)
	e.setRoot(t, e.canonical, 9)
	e.setRoot(t, e.regional, 2)
	if err := e.ctl.CheckAdvertise(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("strong-current gate must never block on lag: %v", err)
	}
	snap := e.ctl.Snapshot()
	if snap.Mode != "strong-current" {
		t.Fatalf("snapshot mode = %q", snap.Mode)
	}
}

func TestSnapshotCountsLaggingRepos(t *testing.T) {
	e := setup(t, replica.ModeBoundedStale, 5*time.Minute, 15*time.Second)
	e.setRoot(t, e.canonical, 8)
	e.setRoot(t, e.regional, 7)
	_ = e.ctl.CheckAdvertise(context.Background(), "acme", "web")
	snap := e.ctl.Snapshot()
	if snap.ReposTracked != 1 || snap.ReposLagging != 1 {
		t.Fatalf("snapshot = %+v, want 1 tracked / 1 lagging", snap)
	}
	if !snap.CanonicalReachable {
		t.Fatal("canonical is reachable")
	}
	if snap.Role != "replica" {
		t.Fatalf("role = %q", snap.Role)
	}
}

func TestParseMode(t *testing.T) {
	if m, ok := replica.ParseMode("strong-current"); !ok || m != replica.ModeStrongCurrent {
		t.Fatal("strong-current")
	}
	if m, ok := replica.ParseMode("bounded-stale"); !ok || m != replica.ModeBoundedStale {
		t.Fatal("bounded-stale")
	}
	if _, ok := replica.ParseMode("eventual"); ok {
		t.Fatal("bad mode must not parse")
	}
}

func TestRefusalMessage(t *testing.T) {
	if got := replica.RefusalMessage(""); got != "this gateway is a read-only replica" {
		t.Fatalf("RefusalMessage('') = %q", got)
	}
	if got := replica.RefusalMessage("https://gw-us.example"); !strings.Contains(got, "push to https://gw-us.example") {
		t.Fatalf("RefusalMessage(url) = %q", got)
	}
}

// failingStore errors on every read (canonical outage simulation).
type failingStore struct{ storage.ObjectStore }

func (failingStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	return nil, errors.New("canonical unreachable")
}

// TestBoundedStaleRegionalOutageDoesNotClearLag verifies that a regional-store
// outage after lag has been detected does NOT reset laggedSince — which would
// make a lagging repo appear healthy (the asymmetric counterpart of the
// canonical-failure guard).
func TestBoundedStaleRegionalOutageDoesNotClearLag(t *testing.T) {
	e := setup(t, replica.ModeBoundedStale, 5*time.Minute, 15*time.Second)
	// canonical v8, regional v7 → lag detected on first check
	e.setRoot(t, e.canonical, 8)
	e.setRoot(t, e.regional, 7)
	_ = e.ctl.CheckAdvertise(context.Background(), "acme", "web") // seeds laggedSince

	// Swap regional store for a failing one — regional outage begins.
	e.ctl.SetRegionalForTest(failingStore{})

	// Advance past check TTL AND past lag budget (6m > 5m).
	e.now = e.now.Add(6 * time.Minute)
	err := e.ctl.CheckAdvertise(context.Background(), "acme", "web")
	var uh *replica.UnhealthyError
	if !errors.As(err, &uh) {
		t.Fatalf("want UnhealthyError (lag budget exceeded) through regional outage, got %v", err)
	}
	if uh.Reason != "lag budget exceeded" {
		t.Fatalf("want reason %q, got %q", "lag budget exceeded", uh.Reason)
	}
}

// TestReplicaHealthTransitionAuditShape drives an unhealthy -> recovered
// transition and asserts both audit events carry audit=true and event==msg,
// the shiplog tap contract.
func TestReplicaHealthTransitionAuditShape(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	ctl := replica.NewController(replica.ControllerConfig{
		Mode:          replica.ModeBoundedStale,
		LagBudget:     5 * time.Minute,
		CheckInterval: 15 * time.Second,
		Regional:      r,
		Canonical:     c,
		Logger:        logger,
		Now:           func() time.Time { return now },
	})

	seed := func(s storage.ObjectStore, version uint64) {
		body := strings.NewReader(rootJSON(version))
		if md, err := s.Head(context.Background(), rootKey); err == nil {
			if _, err := s.PutIfVersionMatches(context.Background(), rootKey, md.Version, body, nil); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := s.PutIfAbsent(context.Background(), rootKey, body, nil); err != nil {
			t.Fatal(err)
		}
	}

	seed(c, 8)
	seed(r, 7)
	_ = ctl.CheckAdvertise(context.Background(), "acme", "web")
	now = now.Add(6 * time.Minute)
	_ = ctl.CheckAdvertise(context.Background(), "acme", "web") // -> unhealthy

	seed(r, 8)
	now = now.Add(16 * time.Second)
	_ = ctl.CheckAdvertise(context.Background(), "acme", "web") // -> recovered

	out := buf.String()
	for _, ev := range []string{"replica.repo.unhealthy", "replica.repo.recovered"} {
		if !strings.Contains(out, ev) {
			t.Fatalf("missing %q transition event in: %s", ev, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, "replica.repo.") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %v (%s)", err, line)
		}
		if a, ok := rec["audit"].(bool); !ok || !a {
			t.Errorf("audit attr missing/not true: %s", line)
		}
		if rec["event"] != rec["msg"] {
			t.Errorf("event (%v) != msg (%v): %s", rec["event"], rec["msg"], line)
		}
	}
}
