package reachability

import (
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
)

// ClassifyFallback returns a short label for the structured warning
// log emitted when upload-pack falls back to the eager-mirror path.
// Bounded label set so dashboards/alerts can pivot on it.
func ClassifyFallback(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoIndex):
		return "no_index"
	case errors.Is(err, deltaindex.ErrMalformed):
		return "delta_decode"
	default:
		return "unknown"
	}
}
