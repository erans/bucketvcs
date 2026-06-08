package buildtrigger

import (
	"context"
	"testing"
)

func TestApply_AzureKinds(t *testing.T) {
	svc, _ := newTestSvc(t)
	doc := `
triggers:
  - tenant: acme
    repo: app
    name: aw
    kind: azurewebhook
    azure_webhook_url: https://dev.azure.com/Org/_apis/public/distributedtask/webhooks/Hook?api-version=6.0-preview
    secret: shared
  - tenant: acme
    repo: app
    name: ap
    kind: azurepipelines
    azure_connector: prod
    azure_project: MyProject
    azure_pipeline_id: 42
`
	res, err := Apply(context.Background(), svc, []byte(doc), false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Created != 2 {
		t.Fatalf("created=%d, want 2", res.Created)
	}
	ap, err := svc.findByName(context.Background(), "acme", "app", "ap")
	if err != nil {
		t.Fatalf("findByName: %v", err)
	}
	if ap.Config.AzurePipelineID != 42 || ap.Config.AzureProject != "MyProject" || ap.Config.AzureConnector != "prod" {
		t.Errorf("azurepipelines config = %+v", ap.Config)
	}
}
