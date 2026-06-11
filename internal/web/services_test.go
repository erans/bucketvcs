package web

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// Compile-time proof that the concrete services satisfy the consumer interfaces
// (signature drift between web and the services breaks this file, not serve.go).
var (
	_ WebhookAdmin = (*webhooks.Service)(nil)
	_ PolicyAdmin  = (*policy.Service)(nil)
	_ HookAdmin    = (*hooks.Store)(nil)
	_ QuotaAdmin   = (*quota.Service)(nil)
	_ TriggerAdmin = (*buildtrigger.Service)(nil)
)

func TestServiceInterfacesCompile(t *testing.T) {} // anchor so the file isn't empty of tests

func TestTriggerServiceSatisfiesTriggerAdmin(t *testing.T) {
	var _ TriggerAdmin = (*buildtrigger.Service)(nil)
}

func TestAuditReaderSatisfiedByReader(t *testing.T) {
	var _ AuditReader = (*auditlog.Reader)(nil)
}
