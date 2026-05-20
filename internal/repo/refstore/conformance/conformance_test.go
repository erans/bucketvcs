package conformance

import "testing"

// TestRefStoreConformance is the single test driver Go's tool picks
// up; it dispatches into RunSuite which contains the property
// subtests. This split lets future consumers reuse RunSuite from
// their own _test.go file without forking the body.
func TestRefStoreConformance(t *testing.T) {
	RunSuite(t)
}
