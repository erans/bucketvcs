package gcs

import (
	"context"
	"fmt"

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
func Open(ctx context.Context, cfg Config) (*GCS, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	var opts []option.ClientOption
	switch {
	case len(cfg.CredentialsJSON) > 0:
		opts = append(opts, option.WithCredentialsJSON(cfg.CredentialsJSON))
	case cfg.CredentialsFile != "":
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

	return &GCS{cfg: cfg, client: client, bucket: bucket}, nil
}

// Close releases the underlying GCS client.
func (g *GCS) Close() error {
	return g.client.Close()
}
