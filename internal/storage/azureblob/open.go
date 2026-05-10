package azureblob

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// Open builds an AzureBlob adapter from cfg. Credential precedence:
//  1. cfg.ConnectionString (full connection string)
//  2. cfg.AccountKey       (Shared Key; also enables SAS URL generation)
//  3. DefaultAzureCredential (env, workload identity, managed identity, CLI)
//
// cfg.ServiceURL, when set, overrides the default account endpoint. CI uses
// this to point at Azurite (http://127.0.0.1:10000/devstoreaccount1).
func Open(ctx context.Context, cfg Config) (*AzureBlob, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	clientOpts := &azblob.ClientOptions{
		ClientOptions: policy.ClientOptions{Retry: retryOpts(cfg)},
	}

	var (
		svc *azblob.Client
		err error
	)
	switch {
	case cfg.ConnectionString != "":
		svc, err = azblob.NewClientFromConnectionString(cfg.ConnectionString, clientOpts)
	case cfg.AccountKey != "":
		cred, kerr := azblob.NewSharedKeyCredential(cfg.Account, cfg.AccountKey)
		if kerr != nil {
			return nil, fmt.Errorf("azureblob: shared key credential: %w", kerr)
		}
		svc, err = azblob.NewClientWithSharedKeyCredential(serviceURL(cfg), cred, clientOpts)
	default:
		cred, derr := azidentity.NewDefaultAzureCredential(nil)
		if derr != nil {
			return nil, fmt.Errorf("azureblob: default credential: %w", derr)
		}
		svc, err = azblob.NewClient(serviceURL(cfg), cred, clientOpts)
	}
	if err != nil {
		return nil, fmt.Errorf("azureblob: new client: %w", err)
	}

	return &AzureBlob{
		cfg:       cfg,
		service:   svc,
		container: svc.ServiceClient().NewContainerClient(cfg.Container),
	}, nil
}

// serviceURL returns either cfg.ServiceURL or the default account URL.
func serviceURL(cfg Config) string {
	if cfg.ServiceURL != "" {
		return cfg.ServiceURL
	}
	return fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.Account)
}

// Close is a no-op; the Azure SDK client holds no persistent connections.
func (a *AzureBlob) Close() error { return nil }
