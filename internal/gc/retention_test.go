package gc_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
)

func TestDefaultRetention_Is7Days(t *testing.T) {
	if got := gc.DefaultRetention; got != 7*24*time.Hour {
		t.Fatalf("DefaultRetention = %v, want 168h", got)
	}
}

func TestRetentionWarning_BelowThreshold(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want bool
	}{
		{30 * time.Second, true},
		{23*time.Hour + 59*time.Minute, true},
		{24 * time.Hour, false},
		{7 * 24 * time.Hour, false},
	}
	for _, c := range cases {
		got := gc.ShouldWarnRetention(c.d)
		if got != c.want {
			t.Errorf("ShouldWarnRetention(%v) = %v, want %v", c.d, got, c.want)
		}
	}
}

func TestRetentionWarning_Message(t *testing.T) {
	msg := gc.RetentionWarning(1 * time.Hour)
	if !strings.Contains(msg, "below 24h") {
		t.Errorf("warning missing 'below 24h': %q", msg)
	}
}
