package s3compat

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Open builds an S3Compat from cfg. The order of operations is:
//  1. applyDefaults (populates tunables; does not mutate Region).
//  2. LoadDefaultConfig (resolves env, profile, instance metadata).
//  3. If cfg.Region is empty, fall back to awsCfg.Region.
//  4. Validate.
//  5. Build the SDK client.
//
// Step 3 ensures profile- and env-supplied regions reach Validate.
//
// Credential precedence:
//  1. Static (cfg.AccessKeyID + cfg.SecretAccessKey [+ SessionToken])
//  2. Shared-config profile (cfg.Profile)
//  3. SDK default chain (env, instance metadata, ...)
func Open(ctx context.Context, cfg Config) (*S3Compat, error) {
	cfg.applyDefaults()

	// Collect SDK config-load options.
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
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

	// If the caller didn't supply a region, fall back to whatever the
	// resolved AWS config picked up (env, profile, instance metadata).
	if cfg.Region == "" {
		cfg.Region = awsCfg.Region
	}

	// Validate AFTER resolving region so profile-supplied regions
	// satisfy the "region is required" check.
	if err := cfg.Validate(); err != nil {
		return nil, err
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
