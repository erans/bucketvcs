package uploadpack_test

import (
	"testing"
)

// TestUploadPack_NoOpFetch_SkipsMirror verifies that when a client's haves
// already include all wanted commits (i.e. the client is up-to-date),
// the lazy negotiation path returns early without materialising the mirror.
//
// TODO: implement this as a full integration test once the engine test
// harness makes it easy to wire a mock mirror.Manager that counts
// EnsureReady / Open calls. The lazy path is exercised at the unit level
// by TestNegotiate_HaveIsTip_ShipsNothing and the serveFetchLazyPath
// logic in service.go; this test is reserved for end-to-end coverage.
func TestUploadPack_NoOpFetch_SkipsMirror(t *testing.T) {
	t.Skip("integration test — implement once engine harness supports mock mirror.Manager; " +
		"lazy path verified via TestNegotiate_HaveIsTip_ShipsNothing + serveFetchLazyPath unit tests")
}
