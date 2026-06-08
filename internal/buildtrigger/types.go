package buildtrigger

import (
	"errors"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Kind identifies how a trigger delivers.
type Kind string

const (
	KindGeneric    Kind = "generic"
	KindCloudBuild Kind = "cloudbuild"
	KindCodeBuild  Kind = "codebuild"
)

// TokenMode controls short-lived token injection.
type TokenMode string

const (
	TokenNone   TokenMode = "none"
	TokenInject TokenMode = "inject"
)

var (
	ErrNotFound       = errors.New("buildtrigger: not found")
	ErrConflict       = errors.New("buildtrigger: trigger already exists")
	ErrInvalidInput   = errors.New("buildtrigger: invalid input")
	ErrReplayInFlight = errors.New("buildtrigger: delivery is in_flight; wait for the attempt to finish")
)

// Trigger is the canonical operator view. Secret is populated only by Create
// (returned once); List/Get return SecretPreview.
type Trigger struct {
	ID            string
	Tenant        string
	Repo          string
	Name          string
	Kind          Kind
	Config        Config
	RefInclude    []string
	RefExclude    []string
	TokenMode     TokenMode
	TokenScopes   auth.TokenScope
	TokenTTL      time.Duration
	Active        bool
	CreatedAt     time.Time
	Secret        string
	SecretPreview string
}

// Config is the kind-specific configuration stored as config_json.
type Config struct {
	URL          string `json:"url,omitempty"`
	Secret       string `json:"secret,omitempty"`
	AWSRegion    string `json:"aws_region,omitempty"`
	AWSProject   string `json:"aws_project,omitempty"`
	AWSConnector string `json:"aws_connector,omitempty"`
}

// TriggerInput is the operator-supplied data for Create.
type TriggerInput struct {
	Tenant      string
	Repo        string
	Name        string
	Kind        Kind
	Config      Config
	RefInclude  []string
	RefExclude  []string
	TokenMode   TokenMode
	TokenScopes auth.TokenScope
	TokenTTL    time.Duration
}

// BuildPayload is the per-matching-ref snapshot enqueued and later rendered.
// Token injection happens at delivery time (kept out of the queue row).
type BuildPayload struct {
	Tenant    string    `json:"tenant"`
	Repo      string    `json:"repo"`
	Actor     string    `json:"actor"`
	TxID      string    `json:"tx_id"`
	HeadOID   string    `json:"head_oid"`
	RefUpdate RefUpdate `json:"ref_update"`
}

// RefUpdate mirrors webhooks.RefUpdate (old/new "0000..." conventions).
type RefUpdate struct {
	Refname string `json:"refname"`
	OldOID  string `json:"old_oid"`
	NewOID  string `json:"new_oid"`
}

// TokenCeiling is the hard upper bound on a trigger's token TTL (reuses M22's
// 1h ceiling). Enforced at Create.
const TokenCeiling = time.Hour
