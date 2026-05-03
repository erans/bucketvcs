package conformance

import "testing"

// runStress is the entry point for the §29 stress tests applicable to
// localfs in M0: 100 concurrent CAS attempts, 10,000 small object
// creates, large multipart pack upload conflict.
func runStress(t *testing.T, f Factory) {
	t.Helper()
	// Stress tests are appended here as Tasks 27–29 implement them.
}
