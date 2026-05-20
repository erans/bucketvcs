package refstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// marshalShardContent encodes refs as the canonical shard-object JSON:
//
//   - Keys sorted lexicographically (so iteration order does not leak
//     into the bytes — load-bearing for content-addressing).
//   - 2-space indent (matches manifest.MarshalBody convention).
//   - No trailing newline.
//   - Empty input encodes to "{}" (not "{\n}").
//
// Determinism is the contract. A future change that introduces extra
// whitespace, alternate quoting, or key reordering MUST bump the
// ref_sharding strategy string and write a migration; old shards
// would otherwise have stable keys but mismatched hashes.
func marshalShardContent(refs map[string]string) ([]byte, error) {
	if len(refs) == 0 {
		return []byte("{}"), nil
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, n := range names {
		nb, err := json.Marshal(n)
		if err != nil {
			return nil, fmt.Errorf("refstore: marshal refname %q: %w", n, err)
		}
		vb, err := json.Marshal(refs[n])
		if err != nil {
			return nil, fmt.Errorf("refstore: marshal oid for %q: %w", n, err)
		}
		buf.WriteString("  ")
		buf.Write(nb)
		buf.WriteString(": ")
		buf.Write(vb)
		if i+1 < len(names) {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// hashShardContent computes the canonical content hash string
// ("sha256-" + 64-lowercase-hex) for shard bytes. This is what goes
// into manifest.RefShard.Hash and into the storage key as
// ".../ref-shards/<this>.json".
func hashShardContent(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256-" + hex.EncodeToString(sum[:])
}
