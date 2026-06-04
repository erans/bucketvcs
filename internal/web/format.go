package web

import (
	"fmt"
	"time"
)

// relTimeAt renders a coarse "2h ago" relative time against now. Zero/negative
// timestamps render "—"; future timestamps (clock skew) clamp to "now".
func relTimeAt(now time.Time, unix int64) string {
	if unix <= 0 {
		return "—"
	}
	d := now.Sub(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// absTime renders the absolute UTC time for tooltips; empty for zero.
func absTime(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04") + " UTC"
}

// humanSize renders byte counts in binary units, one decimal under 10.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}[exp]
	if val < 10 {
		return fmt.Sprintf("%.1f %s", val, suffix)
	}
	return fmt.Sprintf("%.0f %s", val, suffix)
}

// diffClass maps a DiffLine.Kind byte to its CSS class.
func diffClass(kind byte) string {
	switch kind {
	case '+':
		return "add"
	case '-':
		return "del"
	default:
		return "ctx"
	}
}
