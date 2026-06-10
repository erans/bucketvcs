package buildtrigger

import (
	"errors"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Kind identifies how a trigger delivers.
type Kind string

const (
	KindGeneric        Kind = "generic"
	KindCloudBuild     Kind = "cloudbuild"
	KindCodeBuild      Kind = "codebuild"
	KindAzureWebhook   Kind = "azurewebhook"
	KindAzurePipelines Kind = "azurepipelines"
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

	// Azure webhook (KindAzureWebhook). Reuses Secret for the HMAC shared
	// secret (SHA-1). AzureSigHeader defaults to "X-Hub-Signature".
	AzureWebhookURL string `json:"azure_webhook_url,omitempty"`
	AzureSigHeader  string `json:"azure_sig_header,omitempty"`

	// Azure Pipelines REST (KindAzurePipelines). AzureConnector names a
	// connector resolved from the server --build-config YAML (holds org URL +
	// PAT); never stored in the authdb.
	AzureConnector  string `json:"azure_connector,omitempty"`
	AzureProject    string `json:"azure_project,omitempty"`
	AzurePipelineID int    `json:"azure_pipeline_id,omitempty"`
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

// EditInput is the operator-supplied data for Edit. Kind, Config (url/secret),
// Tenant, and Repo are immutable on edit — change kind by delete+recreate.
type EditInput struct {
	Name        string
	RefInclude  []string
	RefExclude  []string
	TokenMode   TokenMode
	TokenScopes auth.TokenScope
	TokenTTL    time.Duration
	Active      bool
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
