package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestEvaluateReachabilityCommits_Triggers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Build two .bvrd files: 600 + 600 commits = 1200 > 1000 threshold.
	var deltaKeys []string
	for i, nCommits := range []int{600, 600} {
		commits := make([]deltaindex.CommitRecord, nCommits)
		for j := range commits {
			// Unique OID per delta to avoid collisions.
			var oid pack.OID
			oid[0] = byte(i + 1)
			oid[1] = byte(j >> 8)
			oid[2] = byte(j)
			commits[j] = deltaindex.CommitRecord{OID: oid, Generation: uint32(j + 1)}
		}
		d := deltaindex.Delta{Commits: commits}
		b, err := deltaindex.Encode(d)
		if err != nil {
			t.Fatalf("Encode delta %d: %v", i, err)
		}
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		key := "deltas/" + hash + ".bvrd"
		if _, err := s.PutIfAbsent(ctx, key, bytes.NewReader(b), nil); err != nil {
			t.Fatalf("PutIfAbsent delta %d: %v", i, err)
		}
		deltaKeys = append(deltaKeys, key)
	}

	body := manifest.Body{
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{
					{Key: deltaKeys[0], Hash: "h1", SizeBytes: 1},
					{Key: deltaKeys[1], Hash: "h2", SizeBytes: 1},
				},
			},
		},
	}

	thr := DefaultThresholds() // ReachabilityDeltaCommits = 1000
	hit, reason, err := EvaluateReachabilityCommits(ctx, s, body, thr)
	if err != nil {
		t.Fatalf("EvaluateReachabilityCommits: %v", err)
	}
	if !hit {
		t.Fatalf("expected hit=true (1200 > 1000 commits)")
	}
	if reason != "delta-commits" {
		t.Errorf("reason = %q, want delta-commits", reason)
	}
}

func TestEvaluateReachabilityCommits_NoTrigger(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 200 commits < 1000 threshold.
	commits := make([]deltaindex.CommitRecord, 200)
	for j := range commits {
		var oid pack.OID
		oid[1] = byte(j >> 8)
		oid[2] = byte(j)
		commits[j] = deltaindex.CommitRecord{OID: oid, Generation: uint32(j + 1)}
	}
	d := deltaindex.Delta{Commits: commits}
	b, err := deltaindex.Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	key := "deltas/" + hash + ".bvrd"
	if _, err := s.PutIfAbsent(ctx, key, bytes.NewReader(b), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	body := manifest.Body{
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{{Key: key, Hash: hash}},
			},
		},
	}

	thr := DefaultThresholds()
	hit, _, err := EvaluateReachabilityCommits(ctx, s, body, thr)
	if err != nil {
		t.Fatalf("EvaluateReachabilityCommits: %v", err)
	}
	if hit {
		t.Fatalf("expected hit=false (200 < 1000 commits)")
	}
}

func TestEvaluateReachabilityCommits_NilReachability(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := manifest.Body{}
	thr := DefaultThresholds()
	hit, reason, err := EvaluateReachabilityCommits(ctx, s, body, thr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hit || reason != "" {
		t.Errorf("nil reachability should not trigger: hit=%v reason=%q", hit, reason)
	}
}
