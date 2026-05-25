package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// RefUpdate is one ref change in a push batch. Mirrors the shape of
// webhooks.RefUpdate so callers in receivepack don't need a converter.
type RefUpdate struct {
	Refname string `json:"refname"`
	OldOID  string `json:"old_oid"`
	NewOID  string `json:"new_oid"`
}

// PreReceivePayload is what a pre-receive hook sees on stdin.
type PreReceivePayload struct {
	Tenant  string
	Repo    string
	BareDir string
	PushID  string // uuid; used as BUCKETVCS_PUSH_ID env
	Actor   string // empty for anonymous
	Updates []RefUpdate
}

// PreReceiveStdin formats the native git pre-receive contract:
//   <oldoid> <newoid> <refname>\n
//   ...
func PreReceiveStdin(p PreReceivePayload) []byte {
	var buf bytes.Buffer
	for _, u := range p.Updates {
		fmt.Fprintf(&buf, "%s %s %s\n", u.OldOID, u.NewOID, u.Refname)
	}
	return buf.Bytes()
}

// PostReceivePayload extends the pre-receive view with the committed state
// (TxID, ManifestVersion, etc.). The runtime fills these from the
// post-Step-10 webhook payload.
type PostReceivePayload struct {
	PreReceivePayload
	TxID            string `json:"tx_id"`
	ManifestVersion int64  `json:"manifest_version"`
	StorageBackend  string `json:"storage_backend"`
}

// PostReceiveStdin formats: native pre-receive lines + blank line + JSON
// envelope. Scripts written for native git work via the line format; M20-aware
// scripts can parse past the blank line and read the JSON for TxID etc.
func PostReceiveStdin(p PostReceivePayload) []byte {
	var buf bytes.Buffer
	for _, u := range p.Updates {
		fmt.Fprintf(&buf, "%s %s %s\n", u.OldOID, u.NewOID, u.Refname)
	}
	buf.WriteByte('\n') // separator
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Tenant          string      `json:"tenant"`
		Repo            string      `json:"repo"`
		PushID          string      `json:"push_id"`
		Actor           string      `json:"actor"`
		TxID            string      `json:"tx_id"`
		ManifestVersion int64       `json:"manifest_version"`
		StorageBackend  string      `json:"storage_backend"`
		Updates         []RefUpdate `json:"updates"`
	}{p.Tenant, p.Repo, p.PushID, p.Actor, p.TxID, p.ManifestVersion, p.StorageBackend, p.Updates})
	return buf.Bytes()
}
