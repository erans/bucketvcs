package reachability

import "errors"

// ErrNoIndex is returned when the manifest's Indexes are insufficient
// to construct a Set (e.g. legacy repo without .bvcg / .bvom).
var ErrNoIndex = errors.New("reachability: manifest has no usable index")
