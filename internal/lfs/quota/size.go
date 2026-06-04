package quota

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseSize parses a byte count with optional binary (KiB/MiB/GiB/TiB) or
// decimal (KB/MB/GB/TB) suffix. Moved from cmd/bucketvcs (M13.5); grammar
// unchanged.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	type unit struct {
		suffix string
		mult   int64
	}
	units := []unit{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1_000_000_000_000}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse %q: %w", s, err)
			}
			if n < 0 {
				return 0, fmt.Errorf("negative value")
			}
			if u.mult > 0 && n > math.MaxInt64/u.mult {
				return 0, fmt.Errorf("value %q overflows int64", s)
			}
			return n * u.mult, nil
		}
	}
	// Bare digit string = bytes.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative value")
	}
	return n, nil
}
