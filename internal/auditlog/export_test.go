package auditlog

import "time"

// SetNow pins the Reader's wall clock for tests in auditlog_test.
func SetNow(r *Reader, f func() time.Time) { r.now = f }
