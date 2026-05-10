package gc

import (
	"fmt"
	"time"
)

// DefaultRetention is the M8 default retention window for sweep
// candidates: 7 days, matching spec §25's hosted-floor language.
const DefaultRetention = 7 * 24 * time.Hour

// RetentionWarnThreshold is the lower bound below which the operator is
// warned that they are racing realistic clone/session lifetimes.
const RetentionWarnThreshold = 24 * time.Hour

// ShouldWarnRetention reports whether d is short enough that we should
// emit an stderr warning.
func ShouldWarnRetention(d time.Duration) bool {
	return d < RetentionWarnThreshold
}

// RetentionWarning returns the human-readable warning message for a
// short retention window.
func RetentionWarning(d time.Duration) string {
	return fmt.Sprintf("WARNING: --retention=%s is below 24h; "+
		"this risks racing in-flight clones and signed URLs. "+
		"Set --retention to at least 24h unless you understand the "+
		"§43.6 race window for this repo.", d)
}
