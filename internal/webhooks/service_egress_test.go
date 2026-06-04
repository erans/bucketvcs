package webhooks_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestCreateRejectsLiteralDeniedIP(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	svc.Egress = &webhooks.EgressPolicy{}
	ctx := context.Background()

	_, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "http://169.254.169.254/latest/meta-data", EventMask: webhooks.EventPush,
	})
	if !errors.Is(err, webhooks.ErrEgressDeniedURL) {
		t.Fatalf("Create(metadata IP): got %v, want ErrEgressDeniedURL", err)
	}

	// Hostname deny pattern also rejects up front.
	svc.Egress = &webhooks.EgressPolicy{DenyHosts: []string{"*.internal.example.com"}}
	_, err = svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "https://ci.internal.example.com/hook", EventMask: webhooks.EventPush,
	})
	if !errors.Is(err, webhooks.ErrEgressDeniedURL) {
		t.Fatalf("Create(denied host): got %v, want ErrEgressDeniedURL", err)
	}

	// Allow hole permits the literal IP.
	svc.Egress = &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "http://127.0.0.1:9999/hook", EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create(allowed loopback): %v", err)
	}

	// nil policy (CLI process) skips the check entirely.
	svc2 := webhooks.New(db)
	if _, err := svc2.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "http://10.0.0.5/hook", EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create(nil policy): %v", err)
	}
}
