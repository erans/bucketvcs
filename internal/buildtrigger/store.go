package buildtrigger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// defaultTokenTTL is the TTL applied when TriggerInput.TokenTTL is zero.
const defaultTokenTTL = 15 * time.Minute

// Service exposes build-trigger management against the M4 authdb.
type Service struct {
	db sqlitestore.Querier
}

// New constructs a Service backed by the given authdb handle.
func New(db sqlitestore.Querier) *Service {
	return &Service{db: db}
}

// Create inserts a new trigger with a server-generated secret (for
// generic/cloudbuild kinds). Returns the Trigger with Secret populated
// (shown once). Subsequent reads return empty Secret + a SecretPreview.
func (s *Service) Create(ctx context.Context, in TriggerInput) (Trigger, error) {
	if in.Tenant == "" {
		return Trigger{}, fmt.Errorf("%w: tenant must not be empty", ErrInvalidInput)
	}
	if in.Repo == "" {
		return Trigger{}, fmt.Errorf("%w: repo must not be empty", ErrInvalidInput)
	}
	if in.Name == "" {
		return Trigger{}, fmt.Errorf("%w: name must not be empty", ErrInvalidInput)
	}
	if !routenames.ValidateName(in.Name) {
		return Trigger{}, fmt.Errorf("%w: invalid name %q", ErrInvalidInput, in.Name)
	}

	switch in.Kind {
	case KindGeneric, KindCloudBuild, KindCodeBuild, KindAzureWebhook, KindAzurePipelines:
	default:
		return Trigger{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, in.Kind)
	}

	// Token TTL: negative or above ceiling rejected; zero defaults.
	ttl := in.TokenTTL
	if ttl < 0 || ttl > TokenCeiling {
		return Trigger{}, fmt.Errorf("%w: token ttl %v out of range (0, %v]", ErrInvalidInput, ttl, TokenCeiling)
	}
	if ttl == 0 {
		ttl = defaultTokenTTL
	}

	// Token mode: default none, except codebuild/azurepipelines default inject.
	mode := in.TokenMode
	if mode == "" {
		if in.Kind == KindCodeBuild || in.Kind == KindAzurePipelines {
			mode = TokenInject
		} else {
			mode = TokenNone
		}
	}
	switch mode {
	case TokenNone, TokenInject:
	default:
		return Trigger{}, fmt.Errorf("%w: unknown token mode %q", ErrInvalidInput, mode)
	}

	// Token scopes: default repo:read + lfs:read.
	scopes := in.TokenScopes
	if scopes == 0 {
		scopes = auth.ScopeRepoRead | auth.ScopeLFSRead
	}

	// Ref globs must be valid path patterns.
	for _, pat := range in.RefInclude {
		if err := policy.ValidatePathPattern(pat); err != nil {
			return Trigger{}, fmt.Errorf("%w: ref_include %q: %s", ErrInvalidInput, pat, err.Error())
		}
	}
	for _, pat := range in.RefExclude {
		if err := policy.ValidatePathPattern(pat); err != nil {
			return Trigger{}, fmt.Errorf("%w: ref_exclude %q: %s", ErrInvalidInput, pat, err.Error())
		}
	}

	cfg := in.Config
	switch in.Kind {
	case KindGeneric, KindCloudBuild:
		if cfg.URL == "" {
			return Trigger{}, fmt.Errorf("%w: %s requires a config url", ErrInvalidInput, in.Kind)
		}
		if cfg.Secret == "" {
			secret, err := generateSecret()
			if err != nil {
				return Trigger{}, fmt.Errorf("buildtrigger: generate secret: %w", err)
			}
			cfg.Secret = secret
		}
	case KindCodeBuild:
		if cfg.AWSRegion == "" || cfg.AWSProject == "" {
			return Trigger{}, fmt.Errorf("%w: codebuild requires aws_region and aws_project", ErrInvalidInput)
		}
	case KindAzureWebhook:
		if cfg.AzureWebhookURL == "" {
			return Trigger{}, fmt.Errorf("%w: azurewebhook requires azure_webhook_url", ErrInvalidInput)
		}
		if !strings.HasPrefix(cfg.AzureWebhookURL, "http://") && !strings.HasPrefix(cfg.AzureWebhookURL, "https://") {
			return Trigger{}, fmt.Errorf("%w: azurewebhook azure_webhook_url must be http or https", ErrInvalidInput)
		}
		// Secret is operator-supplied (must match the Azure service-connection
		// secret) and is NOT auto-generated; an empty secret means unsigned.
	case KindAzurePipelines:
		if cfg.AzureConnector == "" || cfg.AzureProject == "" || cfg.AzurePipelineID <= 0 {
			return Trigger{}, fmt.Errorf("%w: azurepipelines requires azure_connector, azure_project, and azure_pipeline_id > 0", ErrInvalidInput)
		}
	}

	id, err := generateID()
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: generate id: %w", err)
	}

	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal config: %w", err)
	}
	refIncJSON, err := json.Marshal(nonNil(in.RefInclude))
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal ref_include: %w", err)
	}
	refExcJSON, err := json.Marshal(nonNil(in.RefExclude))
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal ref_exclude: %w", err)
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO build_triggers
		   (id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		    token_mode, token_scopes, token_ttl_seconds, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		id, in.Tenant, in.Repo, in.Name, string(in.Kind), configJSON, refIncJSON, refExcJSON,
		string(mode), int64(scopes), int64(ttl/time.Second), now,
	)
	if err != nil {
		if s.db.IsUniqueViolation(err) {
			return Trigger{}, ErrConflict
		}
		return Trigger{}, fmt.Errorf("buildtrigger: insert trigger: %w", err)
	}

	return Trigger{
		ID:          id,
		Tenant:      in.Tenant,
		Repo:        in.Repo,
		Name:        in.Name,
		Kind:        in.Kind,
		Config:      cfg,
		RefInclude:  nonNil(in.RefInclude),
		RefExclude:  nonNil(in.RefExclude),
		TokenMode:   mode,
		TokenScopes: scopes,
		TokenTTL:    ttl,
		Active:      true,
		CreatedAt:   time.Unix(now, 0),
		Secret:      cfg.Secret,
	}, nil
}

// List returns all triggers for (tenant, repo) ordered by name. Secret is
// hidden; SecretPreview is the first 6 chars of the decoded Config.Secret
// (empty for codebuild, which has no secret).
func (s *Service) List(ctx context.Context, tenant, repo string) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		        token_mode, token_scopes, token_ttl_seconds, active, created_at
		 FROM build_triggers
		 WHERE tenant=? AND repo=?
		 ORDER BY name ASC`,
		tenant, repo)
	if err != nil {
		return nil, fmt.Errorf("buildtrigger: list: %w", err)
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		tr, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// Get returns one trigger by id (Secret hidden, SecretPreview populated).
func (s *Service) Get(ctx context.Context, id string) (Trigger, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		        token_mode, token_scopes, token_ttl_seconds, active, created_at
		 FROM build_triggers WHERE id=?`, id)
	tr, err := scanTrigger(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Trigger{}, ErrNotFound
		}
		return Trigger{}, fmt.Errorf("buildtrigger: get %s: %w", id, err)
	}
	return tr, nil
}

// Edit updates the safe (non-kind, non-config) fields of an existing trigger:
// name, ref globs, token mode/scopes/ttl, active. Kind, URL, and secret are
// left untouched (change kind by delete+recreate). Normalization mirrors
// Create (ttl 0 => default, mode "" => kind-aware default, scopes 0 =>
// repo:read|lfs:read). Returns the updated Trigger (Secret hidden), ErrNotFound
// if no row matched, ErrConflict on a name collision, ErrInvalidInput on bad
// input.
func (s *Service) Edit(ctx context.Context, id string, in EditInput) (Trigger, error) {
	existing, err := s.Get(ctx, id) // ErrNotFound propagates
	if err != nil {
		return Trigger{}, err
	}
	if in.Name == "" || !routenames.ValidateName(in.Name) {
		return Trigger{}, fmt.Errorf("%w: invalid name %q", ErrInvalidInput, in.Name)
	}
	ttl := in.TokenTTL
	if ttl < 0 || ttl > TokenCeiling {
		return Trigger{}, fmt.Errorf("%w: token ttl %v out of range (0, %v]", ErrInvalidInput, ttl, TokenCeiling)
	}
	if ttl == 0 {
		ttl = defaultTokenTTL
	}
	mode := in.TokenMode
	if mode == "" {
		if existing.Kind == KindCodeBuild || existing.Kind == KindAzurePipelines {
			mode = TokenInject
		} else {
			mode = TokenNone
		}
	}
	switch mode {
	case TokenNone, TokenInject:
	default:
		return Trigger{}, fmt.Errorf("%w: unknown token mode %q", ErrInvalidInput, mode)
	}
	scopes := in.TokenScopes
	if scopes == 0 {
		scopes = auth.ScopeRepoRead | auth.ScopeLFSRead
	}
	for _, pat := range in.RefInclude {
		if err := policy.ValidatePathPattern(pat); err != nil {
			return Trigger{}, fmt.Errorf("%w: ref_include %q: %s", ErrInvalidInput, pat, err.Error())
		}
	}
	for _, pat := range in.RefExclude {
		if err := policy.ValidatePathPattern(pat); err != nil {
			return Trigger{}, fmt.Errorf("%w: ref_exclude %q: %s", ErrInvalidInput, pat, err.Error())
		}
	}
	refIncJSON, err := json.Marshal(nonNil(in.RefInclude))
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal ref_include: %w", err)
	}
	refExcJSON, err := json.Marshal(nonNil(in.RefExclude))
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal ref_exclude: %w", err)
	}
	active := 0
	if in.Active {
		active = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE build_triggers
		   SET name=?, ref_include=?, ref_exclude=?,
		       token_mode=?, token_scopes=?, token_ttl_seconds=?, active=?
		 WHERE id=?`,
		in.Name, refIncJSON, refExcJSON,
		string(mode), int64(scopes), int64(ttl/time.Second), active, id)
	if err != nil {
		if s.db.IsUniqueViolation(err) {
			return Trigger{}, ErrConflict
		}
		return Trigger{}, fmt.Errorf("buildtrigger: edit %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: edit %s rows affected: %w", id, err)
	}
	if n == 0 {
		return Trigger{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

// RotateSecret regenerates the HMAC shared secret for a generic/cloudbuild
// trigger and returns it once. Errors with ErrInvalidInput for kinds with no
// server-owned secret (codebuild/azurepipelines use connector creds;
// azurewebhook's secret is operator-supplied). Returns ErrNotFound if absent.
func (s *Service) RotateSecret(ctx context.Context, id string) (string, error) {
	var kind string
	var configJSON []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT kind, config_json FROM build_triggers WHERE id=?`, id).Scan(&kind, &configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("buildtrigger: rotate %s read: %w", id, err)
	}
	if Kind(kind) != KindGeneric && Kind(kind) != KindCloudBuild {
		return "", fmt.Errorf("%w: rotate-secret only applies to generic/cloudbuild triggers", ErrInvalidInput)
	}
	var cfg Config
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return "", fmt.Errorf("buildtrigger: rotate %s decode config: %w", id, err)
		}
	}
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("buildtrigger: generate secret: %w", err)
	}
	cfg.Secret = secret
	newJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("buildtrigger: rotate %s marshal config: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE build_triggers SET config_json=? WHERE id=?`, newJSON, id); err != nil {
		return "", fmt.Errorf("buildtrigger: rotate %s update: %w", id, err)
	}
	return secret, nil
}

// Remove deletes a trigger by id. Returns ErrNotFound if no row matched.
func (s *Service) Remove(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM build_triggers WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("buildtrigger: remove %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("buildtrigger: remove %s rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Enable flips active=1.
func (s *Service) Enable(ctx context.Context, id string) error {
	return s.setActive(ctx, id, true)
}

// Disable flips active=0.
func (s *Service) Disable(ctx context.Context, id string) error {
	return s.setActive(ctx, id, false)
}

func (s *Service) setActive(ctx context.Context, id string, active bool) error {
	v := 0
	if active {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE build_triggers SET active=? WHERE id=?`, v, id)
	if err != nil {
		return fmt.Errorf("buildtrigger: set active %s=%v: %w", id, active, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("buildtrigger: set active %s=%v rows affected: %w", id, active, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTrigger decodes one row into a Trigger with Secret hidden and
// SecretPreview populated.
func scanTrigger(sc rowScanner) (Trigger, error) {
	var (
		tr         Trigger
		kind       string
		configJSON []byte
		refIncJSON []byte
		refExcJSON []byte
		mode       string
		scopes     int64
		ttlSeconds int64
		active     int
		createdAt  int64
	)
	if err := sc.Scan(&tr.ID, &tr.Tenant, &tr.Repo, &tr.Name, &kind, &configJSON, &refIncJSON, &refExcJSON,
		&mode, &scopes, &ttlSeconds, &active, &createdAt); err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: scan row: %w", err)
	}
	tr.Kind = Kind(kind)
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &tr.Config); err != nil {
			return Trigger{}, fmt.Errorf("buildtrigger: decode config: %w", err)
		}
	}
	if len(refIncJSON) > 0 {
		if err := json.Unmarshal(refIncJSON, &tr.RefInclude); err != nil {
			return Trigger{}, fmt.Errorf("buildtrigger: decode ref_include: %w", err)
		}
	} else {
		tr.RefInclude = []string{}
	}
	if len(refExcJSON) > 0 {
		if err := json.Unmarshal(refExcJSON, &tr.RefExclude); err != nil {
			return Trigger{}, fmt.Errorf("buildtrigger: decode ref_exclude: %w", err)
		}
	} else {
		tr.RefExclude = []string{}
	}
	tr.TokenMode = TokenMode(mode)
	tr.TokenScopes = auth.TokenScope(scopes)
	tr.TokenTTL = time.Duration(ttlSeconds) * time.Second
	tr.Active = active == 1
	tr.CreatedAt = time.Unix(createdAt, 0)
	tr.SecretPreview = secretPreview(tr.Config.Secret)
	// Secret is never exposed on reads.
	tr.Config.Secret = ""
	return tr, nil
}

// findByName looks up a trigger by (tenant, repo, name). Returns ErrNotFound
// when no row matches.
func (s *Service) findByName(ctx context.Context, tenant, repo, name string) (Trigger, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		        token_mode, token_scopes, token_ttl_seconds, active, created_at
		 FROM build_triggers WHERE tenant=? AND repo=? AND name=?`,
		tenant, repo, name)
	tr, err := scanTrigger(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Trigger{}, ErrNotFound
		}
		return Trigger{}, fmt.Errorf("buildtrigger: findByName %s/%s/%s: %w", tenant, repo, name, err)
	}
	return tr, nil
}

// nonNil returns s, or an empty non-nil slice so JSON encodes [] not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// generateSecret returns 32 random bytes encoded as base64-url-no-padding.
func generateSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// generateIDWithPrefix returns a prefixed id from 12 random bytes encoded as
// base64-url-no-padding. Both trigger ids ("bvbt_") and delivery ids ("bvbd_")
// use this helper.
func generateIDWithPrefix(prefix string) (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// generateID returns a "bvbt_"-prefixed trigger id.
func generateID() (string, error) {
	return generateIDWithPrefix("bvbt_")
}

func secretPreview(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) < 6 {
		return secret
	}
	return secret[:6]
}
