package s3compat

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Open builds an S3Compat from cfg. cfg.Validate must succeed; defaults
// are applied here so callers do not need to call applyDefaults
// themselves.
//
// Credential precedence:
//  1. Static (cfg.AccessKeyID + cfg.SecretAccessKey [+ SessionToken])
//  2. Shared-config profile (cfg.Profile)
//  3. SDK default chain (env, instance metadata, ...)
//
// Open calls applyDefaults BEFORE Validate so Prefix normalization is
// applied before any field check (per the Validate docstring).
func Open(ctx context.Context, cfg Config) (*S3Compat, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	switch {
	case cfg.AccessKeyID != "":
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken,
			),
		))
	case cfg.Profile != "":
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3compat: load AWS config: %w", err)
	}
	awsCfg.Retryer = func() aws.Retryer { return newRetryer(cfg.MaxRetries) }

	clientOpts := []func(*s3.Options){
		func(o *s3.Options) { o.UsePathStyle = cfg.ForcePathStyle },
	}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &S3Compat{
		cfg:     cfg,
		client:  client,
		presign: s3.NewPresignClient(client),
	}, nil
}
