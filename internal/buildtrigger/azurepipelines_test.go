package buildtrigger

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNewAzurePipelinesClientFactory_ResolvesAndErrors(t *testing.T) {
	f := newAzurePipelinesClientFactory(map[string]AzureConnector{
		"prod":  {OrgURL: "https://dev.azure.com/Org", PAT: "p"},
		"nopat": {OrgURL: "https://dev.azure.com/Org"},
	}, http.DefaultClient)

	if _, err := f(Trigger{Config: Config{AzureConnector: "missing"}}); err == nil {
		t.Error("want error for unknown connector name")
	}
	if _, err := f(Trigger{Config: Config{AzureConnector: "nopat"}}); err == nil {
		t.Error("want error for connector missing pat")
	}
	conn, err := f(Trigger{Config: Config{AzureConnector: "prod"}})
	if err != nil {
		t.Fatalf("resolve prod: %v", err)
	}
	if conn.orgURL != "https://dev.azure.com/Org" || conn.pat != "p" || conn.client == nil {
		t.Errorf("resolved conn = %+v (client nil=%v)", conn, conn.client == nil)
	}
}

func TestBuildAzureRunBody_ShapeAndSecretFlag(t *testing.T) {
	p := BuildPayload{
		Tenant: "acme", Repo: "app", Actor: "tester", TxID: "tx-test",
		HeadOID:   "1111111111111111111111111111111111111111",
		RefUpdate: RefUpdate{Refname: "refs/heads/main", OldOID: "0", NewOID: "1111111111111111111111111111111111111111"},
	}
	body, err := buildAzureRunBody(p, "bvts_TESTTOKEN")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	var got struct {
		Resources struct {
			Repositories struct {
				Self struct {
					RefName string `json:"refName"`
				} `json:"self"`
			} `json:"repositories"`
		} `json:"resources"`
		Variables map[string]struct {
			Value    string `json:"value"`
			IsSecret bool   `json:"isSecret"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Resources.Repositories.Self.RefName != "refs/heads/main" {
		t.Errorf("refName=%q, want refs/heads/main", got.Resources.Repositories.Self.RefName)
	}
	want := map[string]string{
		"BV_REPO": "acme/app", "BV_REF": "refs/heads/main",
		"BV_COMMIT": "1111111111111111111111111111111111111111",
		"BV_ACTOR":  "tester", "BV_TX_ID": "tx-test", "BVTS_TOKEN": "bvts_TESTTOKEN",
	}
	for k, v := range want {
		if got.Variables[k].Value != v {
			t.Errorf("var %s=%q, want %q", k, got.Variables[k].Value, v)
		}
	}
	if !got.Variables["BVTS_TOKEN"].IsSecret {
		t.Error("BVTS_TOKEN must have isSecret=true")
	}
	if got.Variables["BV_REPO"].IsSecret {
		t.Error("BV_REPO must not be secret")
	}
}

func TestBuildAzureRunBody_NoTokenOmitsBVTS(t *testing.T) {
	body, err := buildAzureRunBody(BuildPayload{Tenant: "a", Repo: "b",
		RefUpdate: RefUpdate{Refname: "refs/heads/x"}}, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Variables map[string]json.RawMessage `json:"variables"`
	}
	_ = json.Unmarshal(body, &got)
	if _, ok := got.Variables["BVTS_TOKEN"]; ok {
		t.Error("BVTS_TOKEN must be absent when no token injected")
	}
}

func TestAzureRunURL(t *testing.T) {
	got := azureRunURL("https://dev.azure.com/MyOrg/", "MyProject", 42)
	want := "https://dev.azure.com/MyOrg/MyProject/_apis/pipelines/42/runs?api-version=7.1"
	if got != want {
		t.Errorf("url=%q, want %q", got, want)
	}
}
