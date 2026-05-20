package refstore_test

import (
	"fmt"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

func TestShardKey_Length(t *testing.T) {
	for _, name := range []string{"refs/heads/main", "refs/tags/v1", ""} {
		got := refstore.ShardKey(name)
		if len(got) != 2 {
			t.Errorf("ShardKey(%q)=%q (len=%d), want length 2", name, got, len(got))
		}
	}
}

func TestShardKey_LowercaseHex(t *testing.T) {
	for i := 0; i < 1000; i++ {
		k := refstore.ShardKey(fmt.Sprintf("refs/heads/branch-%d", i))
		for _, b := range k {
			if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
				t.Errorf("ShardKey(%d)=%q has non-lowercase-hex byte %q", i, k, b)
			}
		}
	}
}

func TestShardKey_Deterministic(t *testing.T) {
	const name = "refs/heads/main"
	got := refstore.ShardKey(name)
	for i := 0; i < 10; i++ {
		if again := refstore.ShardKey(name); again != got {
			t.Fatalf("non-deterministic: first=%q later=%q", got, again)
		}
	}
}

func TestShardKey_DistributionRoughlyUniform(t *testing.T) {
	// With 10000 distinct refnames and 256 buckets, average bucket
	// load is ~39. A bucket with <10 or >100 entries is a strong
	// signal the distribution is non-uniform. This is a smoke test
	// against a hash regression, not a statistical hypothesis test.
	counts := make(map[string]int, 256)
	for i := 0; i < 10000; i++ {
		counts[refstore.ShardKey(fmt.Sprintf("refs/heads/branch-%d", i))]++
	}
	for k, c := range counts {
		if c < 10 || c > 100 {
			t.Errorf("bucket %q has %d entries (expected ~39); distribution may have regressed", k, c)
		}
	}
	if len(counts) < 250 {
		t.Errorf("only %d distinct shards populated; expected near 256", len(counts))
	}
}
