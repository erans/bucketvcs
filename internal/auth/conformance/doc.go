// Package conformance is a portable test suite that any auth.Store
// implementation must pass. M4 runs it against sqlitestore.Store; later
// hosted implementations (e.g., Postgres) will subscribe via the same
// Run(t, factory) entry point.
package conformance
