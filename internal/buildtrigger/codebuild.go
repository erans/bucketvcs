package buildtrigger

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

// startBuildAPI is the minimal CodeBuild surface this deliverer needs. It
// matches the SDK *codebuild.Client.StartBuild signature exactly so the real
// client satisfies it, and so tests can inject a fake.
type startBuildAPI interface {
	StartBuild(ctx context.Context, in *codebuild.StartBuildInput, optFns ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error)
}

// AWSConnector is operator-level AWS configuration shared across triggers via a
// named connector. The ambient credential chain (env, profile, instance/role
// metadata) is preferred; static keys are a fallback for environments that
// cannot use it.
type AWSConnector struct {
	Region    string
	Profile   string
	AccessKey string
	SecretKey string
}

// codeBuildDeliverer starts an AWS CodeBuild build via SigV4 StartBuild. The
// clientFor and mintFn fields are injectable so tests can fake both the AWS
// client and token minting.
type codeBuildDeliverer struct {
	clientFor func(Trigger) (startBuildAPI, error)
	mintFn    MintFunc
}

func (d *codeBuildDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	client, err := d.clientFor(tr)
	if err != nil {
		return 0, fmt.Errorf("codebuild client: %w", err)
	}

	repo := p.Tenant + "/" + p.Repo
	envOverrides := []cbtypes.EnvironmentVariable{
		{
			Name:  aws.String("BV_REF"),
			Value: aws.String(p.RefUpdate.Refname),
			Type:  cbtypes.EnvironmentVariableTypePlaintext,
		},
		{
			Name:  aws.String("BV_REPO"),
			Value: aws.String(repo),
			Type:  cbtypes.EnvironmentVariableTypePlaintext,
		},
		{
			Name:  aws.String("BV_COMMIT"),
			Value: aws.String(p.HeadOID),
			Type:  cbtypes.EnvironmentVariableTypePlaintext,
		},
	}

	if tr.TokenMode == TokenInject {
		token, err := d.mintFn(ctx, tr, p)
		if err != nil {
			// Retryable: a transient mint failure should be retried per the
			// backoff schedule rather than dropping the build.
			return 0, fmt.Errorf("mint token: %w", err)
		}
		envOverrides = append(envOverrides, cbtypes.EnvironmentVariable{
			Name:  aws.String("BVTS_TOKEN"),
			Value: aws.String(token),
			Type:  cbtypes.EnvironmentVariableTypePlaintext,
		})
	}

	// Prefer the resolved head OID; fall back to the ref's new OID when the
	// payload's HeadOID is empty.
	sourceVersion := p.HeadOID
	if sourceVersion == "" {
		sourceVersion = p.RefUpdate.NewOID
	}

	out, err := client.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName:                  aws.String(tr.Config.AWSProject),
		SourceVersion:                aws.String(sourceVersion),
		EnvironmentVariablesOverride: envOverrides,
	})
	if err != nil {
		return 0, fmt.Errorf("codebuild StartBuild: %w", err)
	}
	_ = out // Build.Id is available in out.Build for future correlation.
	return 200, nil
}

// newCodeBuildClientFactory builds a clientFor that resolves a real CodeBuild
// client per trigger, honoring an optional named connector for region/profile/
// static-credential overrides.
func newCodeBuildClientFactory(connectors map[string]AWSConnector) func(Trigger) (startBuildAPI, error) {
	return func(tr Trigger) (startBuildAPI, error) {
		conn, hasConn := connectors[tr.Config.AWSConnector]

		// Region precedence: trigger config, overridden by the connector when set.
		region := tr.Config.AWSRegion
		if hasConn && conn.Region != "" {
			region = conn.Region
		}

		opts := []func(*awsconfig.LoadOptions) error{}
		if region != "" {
			opts = append(opts, awsconfig.WithRegion(region))
		}
		// Credential precedence: static connector keys (fallback), then shared
		// profile, then the SDK default ambient chain (env, instance/role
		// metadata). Ambient is preferred; static keys exist only for
		// environments that cannot use the chain.
		switch {
		case hasConn && conn.AccessKey != "":
			opts = append(opts, awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(conn.AccessKey, conn.SecretKey, ""),
			))
		case hasConn && conn.Profile != "":
			opts = append(opts, awsconfig.WithSharedConfigProfile(conn.Profile))
		}

		cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
		if err != nil {
			return nil, fmt.Errorf("load AWS config: %w", err)
		}
		return codebuild.NewFromConfig(cfg), nil
	}
}
