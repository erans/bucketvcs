package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/byob"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const tenantUsage = `Usage: bucketvcs tenant <object> <action> [flags]

Objects + actions:
  storage bind    --auth-db=<path> --tenant=<t> --store=<url>
                  --creds-file=<path> --byob-encryption-key=<path>
  storage unbind  --auth-db=<path> --tenant=<t>
  storage list    --auth-db=<path>
  storage verify  --auth-db=<path> --tenant=<t> --byob-encryption-key=<path>

Storage URL schemes:
  localfs:<path>                      Local filesystem (for testing)
  s3://<bucket>[/<prefix>]            AWS S3
  r2://<bucket>[/<prefix>]            Cloudflare R2
  gcs://<bucket>[/<prefix>]           Google Cloud Storage
  azureblob://<container>[/<prefix>]  Azure Blob Storage

Exit codes:
  0  ok
  1  operational error (db unreachable, store probe failed, ...)
  2  usage error (bad flags, missing required flag, ...)
`

func runTenant(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, tenantUsage)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, tenantUsage)
		return 0
	}
	switch args[0] {
	case "storage":
		return runTenantStorage(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tenant: unknown object %q\n%s", args[0], tenantUsage)
		return 2
	}
}

func runTenantStorage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tenant storage: action required (bind|unbind|list|verify)")
		return 2
	}
	switch args[0] {
	case "bind":
		return runTenantStorageBind(ctx, args[1:], stdout, stderr)
	case "unbind":
		return runTenantStorageUnbind(ctx, args[1:], stdout, stderr)
	case "list":
		return runTenantStorageList(ctx, args[1:], stdout, stderr)
	case "verify":
		return runTenantStorageVerify(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tenant storage: unknown action %q\n", args[0])
		return 2
	}
}

func runTenantStorageBind(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant storage bind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	storeURL := fs.String("store", "", "Store URL (required, e.g. s3://bucket)")
	credsFile := fs.String("creds-file", "", "Path to credentials JSON file (required)")
	keyFile := fs.String("byob-encryption-key", "", "Path to 32-byte encryption key file (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *storeURL == "" || *credsFile == "" || *keyFile == "" {
		fmt.Fprintln(stderr, "tenant storage bind: --auth-db, --tenant, --store, --creds-file, --byob-encryption-key required")
		return 2
	}

	// Read and validate encryption key.
	keyBytes, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage bind: read key file: %v\n", err)
		return 1
	}
	if len(keyBytes) < 32 {
		fmt.Fprintf(stderr, "tenant storage bind: encryption key must be >= 32 bytes, got %d\n", len(keyBytes))
		return 2
	}

	// Read and validate credentials JSON.
	credsBytes, err := os.ReadFile(*credsFile)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage bind: read creds file: %v\n", err)
		return 1
	}
	if !json.Valid(credsBytes) {
		fmt.Fprintf(stderr, "tenant storage bind: creds file is not valid JSON\n")
		return 2
	}

	// Open the store with credentials and probe it.
	store, err := openStoreWithCreds(*storeURL, credsBytes)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage bind: open store: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "Probing tenant store...")
	probeErr := runStorageProbe(ctx, store)
	closeStore(store) // release lock before writing to authdb
	if probeErr != nil {
		fmt.Fprintf(stderr, "tenant storage bind: storage probe failed: %v\n", probeErr)
		return 1
	}

	// Encrypt the credentials.
	encCreds, err := byob.Encrypt(keyBytes[:32], credsBytes)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage bind: encrypt creds: %v\n", err)
		return 1
	}

	// Open auth DB and upsert binding.
	authStore, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage bind: open authdb: %v\n", err)
		return 1
	}
	defer authStore.Close()

	now := time.Now().Unix()
	provider := inferProvider(*storeURL)
	binding := sqlitestore.StorageBinding{
		Tenant:     *tenant,
		StoreURL:   *storeURL,
		CredsJSON:  encCreds,
		Provider:   provider,
		CreatedAt:  now,
		UpdatedAt:  now,
		VerifiedAt: now,
	}
	if err := authStore.UpsertStorageBinding(ctx, binding); err != nil {
		fmt.Fprintf(stderr, "tenant storage bind: upsert binding: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "tenant=%s store=%s provider=%s bound\n", *tenant, *storeURL, provider)
	return 0
}

func runTenantStorageUnbind(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant storage unbind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" {
		fmt.Fprintln(stderr, "tenant storage unbind: --auth-db, --tenant required")
		return 2
	}

	authStore, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage unbind: open authdb: %v\n", err)
		return 1
	}
	defer authStore.Close()

	if err := authStore.DeleteStorageBinding(ctx, *tenant); err != nil {
		fmt.Fprintf(stderr, "tenant storage unbind: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "tenant=%s unbound\n", *tenant)
	return 0
}

func runTenantStorageList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant storage list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "tenant storage list: --auth-db required")
		return 2
	}

	authStore, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage list: open authdb: %v\n", err)
		return 1
	}
	defer authStore.Close()

	bindings, err := authStore.ListStorageBindings(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage list: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	for _, b := range bindings {
		row := map[string]any{
			"tenant":      b.Tenant,
			"store_url":   b.StoreURL,
			"provider":    b.Provider,
			"verified_at": b.VerifiedAt,
			"updated_at":  b.UpdatedAt,
		}
		_ = enc.Encode(row)
	}
	return 0
}

func runTenantStorageVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant storage verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	keyFile := fs.String("byob-encryption-key", "", "Path to 32-byte encryption key file (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *keyFile == "" {
		fmt.Fprintln(stderr, "tenant storage verify: --auth-db, --tenant, --byob-encryption-key required")
		return 2
	}

	// Read and validate encryption key.
	keyBytes, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage verify: read key file: %v\n", err)
		return 1
	}
	if len(keyBytes) < 32 {
		fmt.Fprintf(stderr, "tenant storage verify: encryption key must be >= 32 bytes, got %d\n", len(keyBytes))
		return 2
	}

	authStore, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage verify: open authdb: %v\n", err)
		return 1
	}
	defer authStore.Close()

	// Fetch existing binding.
	binding, err := authStore.GetStorageBinding(ctx, *tenant)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage verify: get binding: %v\n", err)
		return 1
	}

	// Decrypt credentials.
	plainCreds, err := byob.Decrypt(keyBytes[:32], binding.CredsJSON)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage verify: decrypt creds: %v\n", err)
		return 1
	}

	// Open the store with decrypted credentials and probe it.
	objStore, err := openStoreWithCreds(binding.StoreURL, plainCreds)
	if err != nil {
		fmt.Fprintf(stderr, "tenant storage verify: open store: %v\n", err)
		return 1
	}

	probeErr := runStorageProbe(ctx, objStore)
	closeStore(objStore) // release lock before writing to authdb
	if probeErr != nil {
		fmt.Fprintf(stderr, "tenant storage verify: storage probe failed: %v\n", probeErr)
		return 1
	}

	// Update verified_at timestamp while preserving created_at.
	now := time.Now().Unix()
	updated := sqlitestore.StorageBinding{
		Tenant:     binding.Tenant,
		StoreURL:   binding.StoreURL,
		CredsJSON:  binding.CredsJSON,
		Provider:   binding.Provider,
		CreatedAt:  binding.CreatedAt,
		UpdatedAt:  now,
		VerifiedAt: now,
	}
	if err := authStore.UpsertStorageBinding(ctx, updated); err != nil {
		fmt.Fprintf(stderr, "tenant storage verify: update binding: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "tenant=%s verified ok\n", *tenant)
	return 0
}

// runStorageProbe performs a PutIfAbsent / Get / DeleteIfVersionMatches round
// trip on a probe key to verify that the store is accessible and writable.
func runStorageProbe(ctx context.Context, store storage.ObjectStore) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generate probe key: %w", err)
	}
	key := "_byob-probe/" + hex.EncodeToString(b)

	ver, err := store.PutIfAbsent(ctx, key, bytes.NewReader([]byte("probe")), nil)
	if err != nil {
		return fmt.Errorf("PutIfAbsent: %w", err)
	}

	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		return fmt.Errorf("Get: %w", err)
	}
	obj.Body.Close()

	if err := store.DeleteIfVersionMatches(ctx, key, ver); err != nil {
		return fmt.Errorf("DeleteIfVersionMatches: %w", err)
	}
	return nil
}

// inferProvider infers the provider name from a store URL scheme prefix.
func inferProvider(storeURL string) string {
	switch {
	case strings.HasPrefix(storeURL, "s3://"):
		return "s3"
	case strings.HasPrefix(storeURL, "r2://"):
		return "r2"
	case strings.HasPrefix(storeURL, "gcs://"):
		return "gcs"
	case strings.HasPrefix(storeURL, "azureblob://"):
		return "azureblob"
	default:
		return "localfs"
	}
}
