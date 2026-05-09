// Package azureblob implements storage.ObjectStore against Azure Blob
// Storage via github.com/Azure/azure-sdk-for-go/sdk/storage/azblob.
// M7 ships this adapter as a canonical bucketvcs storage backend
// (§11.1).
//
// The CLI exposes one scheme that routes to this package:
//
//	azureblob://<container>[/<prefix>]
//
// Credentials come from azidentity.NewDefaultAzureCredential by default
// (env vars, workload identity, managed identity, az CLI). Static keys
// can be supplied via Config.AccountKey or Config.ConnectionString
// (the latter primarily for Azurite). Credentials are never URL-embedded.
//
// Block blobs are used exclusively. Multipart uploads map to the
// StageBlock + CommitBlockList flow with If-None-Match: "*" on the
// commit (the §29 #8 invariant).
//
// See docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md.
package azureblob
