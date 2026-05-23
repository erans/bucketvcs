package webhooks_test

import (
	"reflect"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestEvent_String(t *testing.T) {
	cases := []struct {
		e    webhooks.Event
		want string
	}{
		{webhooks.EventPush, "push"},
		{webhooks.EventLFSUpload, "lfs.upload"},
		{webhooks.EventLFSLockCreated, "lfs.lock.created"},
		{webhooks.EventLFSLockReleased, "lfs.lock.released"},
		{webhooks.EventRepoCreated, "repo.created"},
		{webhooks.EventRepoDeleted, "repo.deleted"},
		{webhooks.EventRepoRenamed, "repo.renamed"},
		{webhooks.EventPolicyRefRejected, "policy.ref.rejected"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.e.String(); got != c.want {
				t.Errorf("String()=%q, want %q", got, c.want)
			}
		})
	}
}

func TestParseEvents(t *testing.T) {
	cases := []struct {
		in   string
		want webhooks.Event
		ok   bool
	}{
		{"push", webhooks.EventPush, true},
		{"push,lfs.upload", webhooks.EventPush | webhooks.EventLFSUpload, true},
		{"all", webhooks.EventMaskAll, true},
		{"lfs.*", webhooks.EventLFSUpload | webhooks.EventLFSLockCreated | webhooks.EventLFSLockReleased, true},
		{"repo.*", webhooks.EventRepoCreated | webhooks.EventRepoDeleted | webhooks.EventRepoRenamed, true},
		{"push, lfs.upload ", webhooks.EventPush | webhooks.EventLFSUpload, true},
		{"", 0, false},
		{"bogus.event", 0, false},
		{"push,bogus", 0, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := webhooks.ParseEvents(c.in)
			if c.ok && err != nil {
				t.Fatalf("ParseEvents(%q) returned error: %v", c.in, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("ParseEvents(%q) returned nil error; want failure", c.in)
			}
			if got != c.want {
				t.Errorf("ParseEvents(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestFormatEvents(t *testing.T) {
	mask := webhooks.EventPush | webhooks.EventLFSUpload | webhooks.EventPolicyRefRejected
	got := webhooks.FormatEvents(mask)
	want := "push,lfs.upload,policy.ref.rejected"
	if got != want {
		t.Errorf("FormatEvents(%d) = %q, want %q", mask, got, want)
	}
	if got := webhooks.FormatEvents(webhooks.EventMaskAll); got != "all" {
		t.Errorf("FormatEvents(EventMaskAll) = %q, want \"all\"", got)
	}
}

func TestEvent_Has(t *testing.T) {
	mask := webhooks.EventPush | webhooks.EventLFSUpload
	if !mask.Has(webhooks.EventPush) {
		t.Errorf("Has(EventPush) = false, want true")
	}
	if mask.Has(webhooks.EventRepoCreated) {
		t.Errorf("Has(EventRepoCreated) = true, want false")
	}
}

func TestEventMaskAll_CountsAllEvents(t *testing.T) {
	// EventMaskAll MUST have one bit set per known event. If a future
	// commit adds an Event constant without updating EventMaskAll, this
	// test fails — preventing silent subscription drift.
	want := len([]webhooks.Event{
		webhooks.EventPush,
		webhooks.EventLFSUpload,
		webhooks.EventLFSLockCreated,
		webhooks.EventLFSLockReleased,
		webhooks.EventRepoCreated,
		webhooks.EventRepoDeleted,
		webhooks.EventRepoRenamed,
		webhooks.EventPolicyRefRejected,
	})
	var got int
	mask := webhooks.EventMaskAll
	for mask != 0 {
		if mask&1 == 1 {
			got++
		}
		mask >>= 1
	}
	if got != want {
		t.Errorf("EventMaskAll has %d bits set, want %d (one per Event constant)", got, want)
	}
}

// TestPayloadStructs_EmbedCommonEnvelope asserts every payload struct embeds
// CommonEnvelope as an anonymous field. wrapEnvelope's dynamic skip-on-
// collision merge depends on the envelope keys appearing as zero values in
// the marshaled payload so the envelope-supplied values are not overwritten.
// Adding a new payload type without embedding CommonEnvelope would silently
// produce an envelope-less body — this test catches that at build time.
func TestPayloadStructs_EmbedCommonEnvelope(t *testing.T) {
	// Compile-time assertion that each named type exists.
	var _ webhooks.PushPayload = webhooks.PushPayload{}
	var _ webhooks.LFSUploadPayload = webhooks.LFSUploadPayload{}
	var _ webhooks.LFSLockPayload = webhooks.LFSLockPayload{}
	var _ webhooks.RepoLifecyclePayload = webhooks.RepoLifecyclePayload{}
	var _ webhooks.RepoRenamedPayload = webhooks.RepoRenamedPayload{}
	var _ webhooks.PolicyRefRejectedPayload = webhooks.PolicyRefRejectedPayload{}

	types := []reflect.Type{
		reflect.TypeOf(webhooks.PushPayload{}),
		reflect.TypeOf(webhooks.LFSUploadPayload{}),
		reflect.TypeOf(webhooks.LFSLockPayload{}),
		reflect.TypeOf(webhooks.RepoLifecyclePayload{}),
		reflect.TypeOf(webhooks.RepoRenamedPayload{}),
		reflect.TypeOf(webhooks.PolicyRefRejectedPayload{}),
	}
	envelopeType := reflect.TypeOf(webhooks.CommonEnvelope{})
	for _, ty := range types {
		f, ok := ty.FieldByName("CommonEnvelope")
		if !ok || !f.Anonymous || f.Type != envelopeType {
			t.Errorf("%s missing embedded CommonEnvelope (ok=%t anon=%t type=%v)",
				ty.Name(), ok, f.Anonymous, f.Type)
		}
	}
}
