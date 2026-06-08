package buildtrigger

import "encoding/json"

// RenderBody produces the JSON POST body for generic/cloudbuild deliveries.
// token is injected as "bvts_token" only when non-empty (TokenInject path).
// The cloudbuild preset additionally flattens commonly-referenced fields
// (ref, commit) to the top level so Cloud Build's $(body.ref) substitution
// mapping is ergonomic.
func RenderBody(kind Kind, p BuildPayload, token string) ([]byte, error) {
	m := map[string]any{
		"tenant":     p.Tenant,
		"repo":       p.Repo,
		"actor":      p.Actor,
		"tx_id":      p.TxID,
		"head_oid":   p.HeadOID,
		"ref_update": p.RefUpdate,
	}
	if kind == KindCloudBuild {
		m["ref"] = p.RefUpdate.Refname
		m["commit"] = p.RefUpdate.NewOID
	}
	if token != "" {
		m["bvts_token"] = token
	}
	return json.Marshal(m)
}
