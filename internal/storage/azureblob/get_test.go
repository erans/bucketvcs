package azureblob

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestVersionFromETagRoundTrip(t *testing.T) {
	raw := azcore.ETag(`"0xABCDEF"`)
	v := versionFromETag(&raw)
	if v.Provider != "azureblob" {
		t.Errorf("Provider = %q, want azureblob", v.Provider)
	}
	if v.Token != "0xABCDEF" {
		t.Errorf("Token = %q, want 0xABCDEF (quotes stripped)", v.Token)
	}
	round := parseETag(v)
	if round != raw {
		t.Errorf("round-trip = %q, want %q", round, raw)
	}
}

func TestParseETagRejectsWrongProvider(t *testing.T) {
	got := parseETag(bvstorage.ObjectVersion{Provider: "gcs", Token: "1"})
	if got != "" {
		t.Errorf("expected empty ETag for wrong provider, got %q", got)
	}
}
