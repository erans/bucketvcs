package gcs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Open builds a GCS adapter from cfg. Credential precedence:
//  1. cfg.CredentialsJSON  (raw bytes)
//  2. cfg.CredentialsFile  (path)
//  3. SDK default chain    (ADC: env, workload identity, metadata)
//
// cfg.Endpoint, when set, overrides the default GCS endpoint. CI uses
// this to point at fake-gcs-server.
//
// When the JSON form is supplied (1 or 2), the service-account
// client_email + private_key are parsed and cached on the adapter so
// SignedGetURL can pass them explicitly to the SDK. This is required
// when STORAGE_EMULATOR_HOST is set (emulator mode skips the SDK's
// credential auto-detect, leaving SignedURL with no GoogleAccessID).
// On real GCS with ADC-derived credentials, the cache is empty and the
// SDK signs via the IAM credentials API on the auto-detected service
// account — same behavior as before this change.
func Open(ctx context.Context, cfg Config) (*GCS, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	var opts []option.ClientOption
	var credsJSON []byte
	switch {
	case len(cfg.CredentialsJSON) > 0:
		credsJSON = cfg.CredentialsJSON
		opts = append(opts, option.WithCredentialsJSON(cfg.CredentialsJSON))
	case cfg.CredentialsFile != "":
		raw, err := os.ReadFile(cfg.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("gcs: read credentials file: %w", err)
		}
		credsJSON = raw
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	}
	if cfg.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(cfg.Endpoint))
	}

	client, err := gstorage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcs: new client: %w", err)
	}

	bucket := client.Bucket(cfg.Bucket)
	if cfg.UserProject != "" {
		bucket = bucket.UserProject(cfg.UserProject)
	}
	bucket = applyRetry(bucket, retryOpts(cfg))

	g := &GCS{cfg: cfg, client: client, bucket: bucket}
	if len(credsJSON) > 0 {
		var sa struct {
			ClientEmail string `json:"client_email"`
			PrivateKey  string `json:"private_key"`
		}
		// Parse failure here is tolerated: ADC supports JSON shapes
		// beyond plain service-account (e.g. external-account /
		// impersonation) that don't carry a private_key. Those paths
		// rely on the SDK's IAM credentials API for signing, so an
		// empty cache is the correct state.
		if err := json.Unmarshal(credsJSON, &sa); err == nil {
			g.signGoogleAccessID = sa.ClientEmail
			g.signPrivateKey = []byte(sa.PrivateKey)
		}
	}
	return g, nil
}

// Close releases the underlying GCS client.
func (g *GCS) Close() error {
	return g.client.Close()
}
