package conformance

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
)

// DeterministicBytes returns n bytes generated from seed. Two calls with
// the same (n, seed) return the same bytes. Used for test fixtures so
// fixture content is stable across runs without inflating the repo.
func DeterministicBytes(n int, seed string) []byte {
	out := make([]byte, n)
	h := fnv.New64a()
	h.Write([]byte(seed))
	state := h.Sum64()
	for i := 0; i < n; i += 8 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		end := i + 8
		if end > n {
			end = n
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], state)
		copy(out[i:end], buf[:end-i])
	}
	return out
}

// Key returns a key derived from a base prefix and an integer suffix,
// formatted so lexicographic order matches numeric order across the
// expected range. Tests use this for predictable List ordering.
func Key(prefix string, n int) string {
	return fmt.Sprintf("%s/%010d", prefix, n)
}
