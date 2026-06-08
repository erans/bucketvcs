package buildtrigger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// AzureConnector is operator-level Azure DevOps configuration shared across
// triggers via a named connector. It holds the organization URL and a Personal
// Access Token; the PAT is sent as HTTP Basic auth (empty username) and never
// stored in the authdb.
type AzureConnector struct {
	OrgURL string `yaml:"org_url"`
	PAT    string `yaml:"pat"`
}

// azureConn is the resolved per-trigger client view used by the deliverer.
type azureConn struct {
	orgURL string
	pat    string
	client *http.Client
}

// azurePipelinesDeliverer queues an Azure Pipelines run via the Run Pipeline
// REST API. clientFor resolves a named connector to an authenticated client;
// mintFn is injectable so tests can fake token minting.
type azurePipelinesDeliverer struct {
	clientFor func(Trigger) (azureConn, error)
	mintFn    MintFunc
}

func (d *azurePipelinesDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	conn, err := d.clientFor(tr)
	if err != nil {
		return 0, fmt.Errorf("azure connector: %w", err)
	}
	var token string
	if tr.TokenMode == TokenInject {
		tok, err := d.mintFn(ctx, tr, p)
		if err != nil {
			return 0, fmt.Errorf("mint token: %w", err)
		}
		token = tok
	}
	body, err := buildAzureRunBody(p, token)
	if err != nil {
		return 0, fmt.Errorf("build body: %w", err)
	}

	url := azureRunURL(conn.orgURL, tr.Config.AzureProject, tr.Config.AzurePipelineID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bucketvcs-buildtrigger/1")
	req.SetBasicAuth("", conn.pat)

	resp, err := conn.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}

// azureRunURL builds the Run Pipeline REST endpoint for an org/project/pipeline.
func azureRunURL(orgURL, project string, pipelineID int) string {
	return strings.TrimRight(orgURL, "/") + "/" + project +
		"/_apis/pipelines/" + strconv.Itoa(pipelineID) + "/runs?api-version=7.1"
}

// azureVar is one entry in the RunPipelineParameters.variables map.
type azureVar struct {
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret,omitempty"`
}

// buildAzureRunBody renders the RunPipelineParameters JSON: the pushed ref is
// pinned via resources.repositories.self.refName, and push metadata is passed
// as BV_* run variables (matching the CodeBuild env-var convention). The
// injected token, when present, is marked isSecret so Azure masks it in logs.
func buildAzureRunBody(p BuildPayload, token string) ([]byte, error) {
	vars := map[string]azureVar{
		"BV_REPO":   {Value: p.Tenant + "/" + p.Repo},
		"BV_REF":    {Value: p.RefUpdate.Refname},
		"BV_COMMIT": {Value: p.HeadOID},
		"BV_ACTOR":  {Value: p.Actor},
		"BV_TX_ID":  {Value: p.TxID},
	}
	if token != "" {
		vars["BVTS_TOKEN"] = azureVar{Value: token, IsSecret: true}
	}
	body := map[string]any{
		"resources": map[string]any{
			"repositories": map[string]any{
				"self": map[string]any{"refName": p.RefUpdate.Refname},
			},
		},
		"variables": vars,
	}
	return json.Marshal(body)
}

// newAzurePipelinesClientFactory builds a clientFor that resolves the named
// connector for a trigger. A missing connector returns an error (the delivery
// then retries on the backoff schedule and dead-letters on exhaustion). The
// shared *http.Client (egress-gated, built in ProductionDeliverers) is reused
// for every connector.
func newAzurePipelinesClientFactory(connectors map[string]AzureConnector, client *http.Client) func(Trigger) (azureConn, error) {
	return func(tr Trigger) (azureConn, error) {
		conn, ok := connectors[tr.Config.AzureConnector]
		if !ok {
			return azureConn{}, fmt.Errorf("unknown azure connector %q", tr.Config.AzureConnector)
		}
		if conn.OrgURL == "" || conn.PAT == "" {
			return azureConn{}, fmt.Errorf("azure connector %q missing org_url or pat", tr.Config.AzureConnector)
		}
		return azureConn{orgURL: conn.OrgURL, pat: conn.PAT, client: client}, nil
	}
}
