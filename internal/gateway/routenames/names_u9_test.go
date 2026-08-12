package routenames

import (
	"strings"
	"testing"
)

func TestValidateName_MatchesDurableKeyContract_U9(t *testing.T) {
	for _, valid := range []string{"a", "Acme_1", "repo-name", strings.Repeat("x", 128)} {
		if !ValidateName(valid) {
			t.Errorf("ValidateName(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", ".", "..", "my.repo", "a..b", "a/b", strings.Repeat("x", 129)} {
		if ValidateName(invalid) {
			t.Errorf("ValidateName(%q) = true", invalid)
		}
	}
}
