package azureblob

import "testing"

func TestMarkerPtr(t *testing.T) {
	if markerPtr("") != nil {
		t.Errorf("markerPtr(\"\"): want nil")
	}
	got := markerPtr("abc")
	if got == nil || *got != "abc" {
		t.Errorf("markerPtr(\"abc\"): want pointer to \"abc\"")
	}
}
