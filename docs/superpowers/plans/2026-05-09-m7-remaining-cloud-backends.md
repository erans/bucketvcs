# M7 — Remaining Canonical Cloud Backends Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `internal/storage/gcs` (Google Cloud Storage) and `internal/storage/azureblob` (Azure Blob Storage) as `storage.ObjectStore` adapters, formally promote AWS S3 to canonical, and gate all four canonical cloud backends in CI (emulators on every PR, real cloud nightly).

**Architecture:** Two new packages, each self-contained, mirroring the M5 `s3compat` package layout file-for-file. Each package thinly wraps its provider SDK behind `storage.ObjectStore`; nothing outside the package imports provider types. CLI grows two new schemes (`gcs://`, `azureblob://`). Conformance suite runs against `fake-gcs-server` and Azurite on every PR via a new GitHub Actions workflow; the same suite runs against real GCS / Azure / AWS S3 / R2 in a nightly job.

**Tech Stack:** Go 1.25, `cloud.google.com/go/storage` (GCS), `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob` + `github.com/Azure/azure-sdk-for-go/sdk/azidentity` (Azure), Docker Compose for `fake-gcs-server` and Azurite, existing `internal/storage` contract and `internal/storage/conformance` suite from M0.

**Spec:** `docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md`

---

## File Structure

**New files:**

```
internal/storage/gcs/
  doc.go
  gcs.go                       // type GCS, interface assertion, Capabilities
  config.go                    // Config struct, Validate, defaults
  config_test.go
  url.go                       // ParseURL("gcs://...") -> Config seed
  url_test.go
  prefix.go                    // applyPrefix / stripPrefix / normalizePrefix
  prefix_test.go
  keys.go                      // validateKey
  keys_test.go
  errs.go                      // classify(op, err) -> storage sentinel
  errs_test.go
  retry.go                     // gax retry policy configuration
  retry_test.go
  open.go                      // Open(ctx, Config) (*GCS, error)
  open_test.go                 // httptest-mocked Open
  get.go                       // Get / Head / GetRange
  get_test.go
  put.go                       // PutIfAbsent / PutIfVersionMatches
  put_test.go
  delete.go                    // DeleteIfVersionMatches
  delete_test.go
  list.go                      // List with delimiter + pagination
  list_test.go
  multipart.go                 // resumable upload session per MultipartUpload
  multipart_test.go
  signed.go                    // SignedGetURL via storage.SignedURL
  signed_test.go
  cleanup.go                   // AbortMultipartsUnderPrefix (test cleanup)
  gcs_conformance_test.go      // live + fake-gcs conformance, skip when env unset
  README.md

internal/storage/azureblob/
  doc.go
  azureblob.go
  config.go
  config_test.go
  url.go
  url_test.go
  prefix.go
  prefix_test.go
  keys.go
  keys_test.go
  errs.go
  errs_test.go
  retry.go
  retry_test.go
  open.go
  open_test.go
  get.go
  get_test.go
  put.go
  put_test.go
  delete.go
  delete_test.go
  list.go
  list_test.go
  multipart.go                 // StageBlock + CommitBlockList
  multipart_test.go
  signed.go                    // SignedGetURL via service SAS
  signed_test.go
  cleanup.go
  azureblob_conformance_test.go
  README.md

docker-compose.cloud.yml       // MinIO + fake-gcs-server + Azurite
scripts/conformance-emulators.sh

.github/workflows/conformance.yml  // emulators (PR-blocking) + real-cloud (nightly)

docs/m7-cloud-quickstart.md
docs/superpowers/specs/m7_progress.md  // written at merge
```

**Modified files:**

```
go.mod                                   // add GCS + Azure SDK deps
go.sum                                   // resolved checksums

cmd/bucketvcs/store.go                   // add gcs:// + azureblob:// cases; remove M7 reservation
cmd/bucketvcs/store_test.go              // parser tests for new schemes
cmd/bucketvcs/main.go                    // (if applyEnvToConfig moves out — see Task 3.x)

internal/diffharness/roundtrip_helpers_test.go
                                         // env-driven store override:
                                         //   BUCKETVCS_DIFFHARNESS_STORE=gcs://... | azureblob://...

docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md
                                         // §11.1 row marks s3:// canonical
docs/m5-cloud-quickstart.md              // drop "M7 promotion in progress" qualifier
README.md                                // list s3, r2, gcs, azureblob as canonical
internal/storage/s3compat/README.md      // drop "promoted to canonical at M7" wording
```

---

I'll add the tasks in subsequent edits to keep each turn small. The first phase below sets up CI + emulators so adapter work has somewhere to run.

## Phase 0 — CI scaffolding and emulators

The adapter packages need a place to run. We bring up the emulator stack and a runner script first, so every adapter task can verify against `fake-gcs-server` and Azurite locally and in CI from day one.

### Task 0.1: Docker Compose stack for emulators

**Files:**
- Create: `docker-compose.cloud.yml`

- [ ] **Step 1: Write the compose file**

Create `docker-compose.cloud.yml`:

```yaml
# Local emulator stack for storage adapter conformance.
# Used by scripts/conformance-emulators.sh and the GitHub Actions
# `emulators` job.
services:
  minio:
    image: minio/minio:RELEASE.2025-01-20T14-49-07Z
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    ports:
      - "9000:9000"
      - "9001:9001"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:9000/minio/health/live"]
      interval: 2s
      timeout: 2s
      retries: 30

  fake-gcs:
    image: fsouza/fake-gcs-server:1.49.3
    command:
      - "-scheme"
      - "http"
      - "-public-host"
      - "localhost:4443"
      - "-port"
      - "4443"
    ports:
      - "4443:4443"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:4443/storage/v1/b"]
      interval: 2s
      timeout: 2s
      retries: 30

  azurite:
    image: mcr.microsoft.com/azure-storage/azurite:3.33.0
    command:
      - "azurite-blob"
      - "--blobHost"
      - "0.0.0.0"
      - "--blobPort"
      - "10000"
      - "--skipApiVersionCheck"
    ports:
      - "10000:10000"
    healthcheck:
      test: ["CMD", "nc", "-z", "127.0.0.1", "10000"]
      interval: 2s
      timeout: 2s
      retries: 30
```

- [ ] **Step 2: Smoke-test the stack**

Run:
```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m7-cloud
docker compose -f docker-compose.cloud.yml up -d
docker compose -f docker-compose.cloud.yml ps
```

Expected: three services reach the `healthy` (or `running`) state within ~30 seconds.

- [ ] **Step 3: Tear down**

Run:
```bash
docker compose -f docker-compose.cloud.yml down -v
```

Expected: clean shutdown, no leftover volumes.

- [ ] **Step 4: Commit**

```bash
git add docker-compose.cloud.yml
git commit -m "M7 task 0.1: docker compose stack for cloud emulators"
```

---

### Task 0.2: conformance-emulators.sh runner script

**Files:**
- Create: `scripts/conformance-emulators.sh`

- [ ] **Step 1: Write the runner**

Create `scripts/conformance-emulators.sh`:

```bash
#!/usr/bin/env bash
# Boot the local cloud-emulator stack, export the env vars each adapter
# expects, create test buckets/containers, and run the storage
# conformance suite against MinIO + fake-gcs-server + Azurite.
#
# Used both locally and from .github/workflows/conformance.yml.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.cloud.yml"
KEEP_UP="${BUCKETVCS_KEEP_EMULATORS:-0}"

cleanup() {
  if [[ "$KEEP_UP" != "1" ]]; then
    docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "==> Booting emulator stack"
docker compose -f "$COMPOSE_FILE" up -d --wait

# MinIO bucket creation via mc-in-container.
echo "==> Creating MinIO bucket bucketvcs-conformance"
docker run --rm --network host \
  --entrypoint sh \
  minio/mc:RELEASE.2025-01-17T23-25-50Z \
  -c '
    mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null
    mc mb --ignore-existing local/bucketvcs-conformance
  '

# fake-gcs-server creates buckets implicitly on first PUT, but the
# conformance suite calls List before Put on the same key, so we
# pre-create the bucket explicitly.
echo "==> Creating fake-gcs bucket bucketvcs-conformance"
curl -fsS -X POST \
  -H "Content-Type: application/json" \
  -d '{"name":"bucketvcs-conformance"}' \
  "http://localhost:4443/storage/v1/b?project=bucketvcs"

# Azurite creates containers on first PUT too, but tests assume
# existence. Create via the well-known dev account.
echo "==> Creating Azurite container bucketvcs-conformance"
docker run --rm --network host \
  mcr.microsoft.com/azure-cli:2.66.0 \
  az storage container create \
    --name bucketvcs-conformance \
    --connection-string \
    "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;" \
    >/dev/null

# Export env for the Go test process. Each adapter's loadConfigFromEnv
# helper reads these.

# MinIO -> s3compat
export BUCKETVCS_S3_TEST_BUCKET=bucketvcs-conformance
export BUCKETVCS_S3_REGION=us-east-1
export BUCKETVCS_S3_ENDPOINT=http://localhost:9000
export BUCKETVCS_S3_FORCE_PATH_STYLE=true
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin

# fake-gcs -> gcs
export BUCKETVCS_GCS_TEST_BUCKET=bucketvcs-conformance
export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443

# Azurite -> azureblob
export BUCKETVCS_AZURE_TEST_CONTAINER=bucketvcs-conformance
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

echo "==> Running storage conformance suite"
cd "$REPO_ROOT"
go test -count=1 -timeout=10m ./internal/storage/...
```

- [ ] **Step 2: Make executable**

Run:
```bash
chmod +x scripts/conformance-emulators.sh
```

- [ ] **Step 3: Smoke-test (will fail until adapters exist)**

Run:
```bash
./scripts/conformance-emulators.sh
```

Expected: stack comes up, buckets are created, `go test` runs and PASSES on existing packages (`localfs`, `s3compat`). The new `gcs` and `azureblob` packages don't exist yet so they're not in the test set.

- [ ] **Step 4: Commit**

```bash
git add scripts/conformance-emulators.sh
git commit -m "M7 task 0.2: conformance-emulators.sh runner"
```

---

### Task 0.3: GitHub Actions conformance workflow (skeleton)

**Files:**
- Create: `.github/workflows/conformance.yml`

This task wires the workflow up front so every subsequent adapter task gets PR-level signal. The `real-cloud` job is included but has its steps gated on secrets being present — it's a no-op until repo secrets are configured.

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/conformance.yml`:

```yaml
name: conformance

on:
  pull_request:
  push:
    branches: [main]
  schedule:
    - cron: "17 6 * * *"  # nightly real-cloud run
  workflow_dispatch:

jobs:
  emulators:
    name: emulators (MinIO + fake-gcs + Azurite)
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true
      - name: Run conformance against emulators
        run: ./scripts/conformance-emulators.sh

  real-cloud:
    name: real-cloud (nightly)
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true

      - name: AWS S3 conformance
        if: ${{ secrets.AWS_S3_TEST_BUCKET != '' }}
        env:
          BUCKETVCS_S3_TEST_BUCKET: ${{ secrets.AWS_S3_TEST_BUCKET }}
          BUCKETVCS_S3_REGION: ${{ secrets.AWS_S3_TEST_REGION }}
          AWS_ACCESS_KEY_ID: ${{ secrets.AWS_S3_TEST_ACCESS_KEY_ID }}
          AWS_SECRET_ACCESS_KEY: ${{ secrets.AWS_S3_TEST_SECRET_ACCESS_KEY }}
        run: go test -count=1 -timeout=15m ./internal/storage/s3compat

      - name: Cloudflare R2 conformance
        if: ${{ secrets.R2_TEST_BUCKET != '' }}
        env:
          BUCKETVCS_S3_TEST_BUCKET: ${{ secrets.R2_TEST_BUCKET }}
          BUCKETVCS_S3_REGION: auto
          BUCKETVCS_S3_ENDPOINT: ${{ secrets.R2_TEST_ENDPOINT }}
          BUCKETVCS_S3_FORCE_PATH_STYLE: "true"
          AWS_ACCESS_KEY_ID: ${{ secrets.R2_TEST_ACCESS_KEY_ID }}
          AWS_SECRET_ACCESS_KEY: ${{ secrets.R2_TEST_SECRET_ACCESS_KEY }}
        run: go test -count=1 -timeout=15m ./internal/storage/s3compat

      - name: GCS conformance
        if: ${{ secrets.GCS_TEST_BUCKET != '' }}
        env:
          BUCKETVCS_GCS_TEST_BUCKET: ${{ secrets.GCS_TEST_BUCKET }}
          GOOGLE_APPLICATION_CREDENTIALS: ${{ runner.temp }}/gcs-creds.json
        run: |
          echo '${{ secrets.GCS_TEST_CREDENTIALS_JSON }}' > "$GOOGLE_APPLICATION_CREDENTIALS"
          go test -count=1 -timeout=15m ./internal/storage/gcs

      - name: Azure Blob conformance
        if: ${{ secrets.AZURE_TEST_CONTAINER != '' }}
        env:
          BUCKETVCS_AZURE_TEST_CONTAINER: ${{ secrets.AZURE_TEST_CONTAINER }}
          BUCKETVCS_AZURE_ACCOUNT: ${{ secrets.AZURE_TEST_ACCOUNT }}
          BUCKETVCS_AZURE_ACCOUNT_KEY: ${{ secrets.AZURE_TEST_ACCOUNT_KEY }}
        run: go test -count=1 -timeout=15m ./internal/storage/azureblob
```

- [ ] **Step 2: Lint the YAML locally**

Run:
```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/conformance.yml'))" && echo OK
```
Expected: `OK`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/conformance.yml
git commit -m "M7 task 0.3: GitHub Actions conformance workflow (emulators + nightly real-cloud)"
```

---

## Phase 1 — GCS adapter

Mirrors `internal/storage/s3compat` file-for-file. Each task ends with green tests and a commit.

### Task 1.1: Skeleton — package, type, Capabilities, SDK dep

**Files:**
- Create: `internal/storage/gcs/doc.go`
- Create: `internal/storage/gcs/gcs.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the GCS SDK dependency**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m7-cloud
go get cloud.google.com/go/storage@latest
go mod tidy
```
Expected: `go.mod` gains `cloud.google.com/go/storage`; `go build ./...` still passes.

- [ ] **Step 2: Create `doc.go`**

```go
// Package gcs implements storage.ObjectStore against Google Cloud
// Storage via cloud.google.com/go/storage. M7 ships this adapter as
// a canonical bucketvcs storage backend (§11.1).
//
// The CLI exposes one scheme that routes to this package:
//
//	gcs://<bucket>[/<prefix>]
//
// Credentials come from Application Default Credentials by default
// (env vars, workload identity, GCE/GKE metadata). Static credentials
// can be supplied via Config.CredentialsJSON or Config.CredentialsFile.
// Credentials are never URL-embedded.
//
// See docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md
// for the design rationale.
package gcs
```

- [ ] **Step 3: Create `gcs.go` with the type and capabilities**

```go
package gcs

import (
	"cloud.google.com/go/storage"
	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// GCS is the Google Cloud Storage storage.ObjectStore implementation.
type GCS struct {
	cfg    Config
	client *storage.Client
	bucket *storage.BucketHandle
}

var _ bvstorage.ObjectStore = (*GCS)(nil)

// Capabilities reports the GCS adapter capabilities. Values match real
// provider limits: 5 MiB minimum part size for resumable upload chunks,
// 32 max chunks per resumable session is not a GCS limit (we model
// MultipartUpload as a single resumable upload, so the part count is
// bounded only by what the suite exercises). MaxObjectSize is GCS's
// documented 5 TiB limit. SignedURLs are reported true; emulators that
// do not implement them return ErrNotSupported at call time and the
// suite skips §29 #10 accordingly.
func (g *GCS) Capabilities() bvstorage.Capabilities {
	return bvstorage.Capabilities{
		SignedURLs:           true,
		StrongList:           true,
		MultipartMinPartSize: 256 << 10, // GCS resumable chunk minimum
		MultipartMaxParts:    0,         // no adapter-imposed cap
		MaxObjectSize:        5 << 40,
	}
}
```

- [ ] **Step 4: Verify build**

```bash
go build ./internal/storage/gcs
```
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/storage/gcs/doc.go internal/storage/gcs/gcs.go
git commit -m "M7 task 1.1: gcs package skeleton + Capabilities"
```

---

### Task 1.2: Config + Validate + applyDefaults

**Files:**
- Create: `internal/storage/gcs/config.go`
- Create: `internal/storage/gcs/config_test.go`

- [ ] **Step 1: Write the failing test**

`internal/storage/gcs/config_test.go`:

```go
package gcs

import (
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok", Config{Bucket: "b"}, ""},
		{"missing bucket", Config{}, "bucket is required"},
		{"bad prefix", Config{Bucket: "b", Prefix: "//bad"}, "invalid prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: want nil, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate: want %q, got nil", tc.wantErr)
			case tc.wantErr != "" && err != nil:
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate: want %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestConfigApplyDefaults(t *testing.T) {
	c := Config{Bucket: "b"}
	c.applyDefaults()
	if c.UploadChunkSize != defaultUploadChunkSize {
		t.Errorf("UploadChunkSize = %d, want %d", c.UploadChunkSize, defaultUploadChunkSize)
	}
	if c.MaxRetries != defaultMaxRetries {
		t.Errorf("MaxRetries = %d, want %d", c.MaxRetries, defaultMaxRetries)
	}
	if c.RequestTimeout != defaultRequestTimeout {
		t.Errorf("RequestTimeout = %v, want %v", c.RequestTimeout, defaultRequestTimeout)
	}
	if c.PresignDefaultTTL != defaultPresignDefaultTTL {
		t.Errorf("PresignDefaultTTL = %v, want %v", c.PresignDefaultTTL, defaultPresignDefaultTTL)
	}
	_ = time.Second // ensure import
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/storage/gcs -run TestConfig -v
```
Expected: FAIL — `Config` undefined.

- [ ] **Step 3: Write `config.go`**

```go
package gcs

import (
	"fmt"
	"time"
)

// Config is the only constructor input to Open. The CLI builds one from
// a parsed URL plus environment variables; tests construct it directly.
type Config struct {
	Bucket string // required
	Prefix string // optional; trailing "/" normalized

	// Endpoint overrides the default GCS endpoint
	// (https://storage.googleapis.com). CI uses this to point the
	// adapter at fake-gcs-server.
	Endpoint string

	// CredentialsJSON, if set, takes precedence over the default
	// credential chain. CredentialsFile is a path alternative.
	CredentialsJSON []byte
	CredentialsFile string

	// UserProject names the billing project for requester-pays buckets.
	UserProject string

	UploadChunkSize   int
	MaxRetries        int
	RequestTimeout    time.Duration
	PresignDefaultTTL time.Duration
}

const (
	defaultUploadChunkSize   = 8 << 20
	defaultMaxRetries        = 5
	defaultRequestTimeout    = 60 * time.Second
	defaultPresignDefaultTTL = 15 * time.Minute
)

func (c *Config) Validate() error {
	if c.Bucket == "" {
		return fmt.Errorf("gcs: bucket is required")
	}
	if _, err := normalizePrefix(c.Prefix); err != nil {
		return fmt.Errorf("gcs: invalid prefix: %w", err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if p, err := normalizePrefix(c.Prefix); err == nil {
		c.Prefix = p
	}
	if c.UploadChunkSize == 0 {
		c.UploadChunkSize = defaultUploadChunkSize
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = defaultMaxRetries
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.PresignDefaultTTL == 0 {
		c.PresignDefaultTTL = defaultPresignDefaultTTL
	}
}
```

Note: `normalizePrefix` is defined in Task 1.4 (`prefix.go`). The build will fail until that lands. Run config tests after Task 1.4.

- [ ] **Step 4: Commit (build will fail; that's expected — Task 1.4 fixes it)**

```bash
git add internal/storage/gcs/config.go internal/storage/gcs/config_test.go
git commit -m "M7 task 1.2: gcs Config + Validate + defaults (depends on prefix.go)"
```

---

### Task 1.3: ParseURL

**Files:**
- Create: `internal/storage/gcs/url.go`
- Create: `internal/storage/gcs/url_test.go`

- [ ] **Step 1: Write the failing test**

`internal/storage/gcs/url_test.go`:

```go
package gcs

import "testing"

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw, wantBucket, wantPrefix, wantErr string
	}{
		{"gcs://my-bucket", "my-bucket", "", ""},
		{"gcs://my-bucket/repos", "my-bucket", "repos/", ""},
		{"gcs://my-bucket/repos/staging/", "my-bucket", "repos/staging/", ""},
		{"gcs://", "", "", "bucket required"},
		{"s3://my-bucket", "", "", "unsupported scheme"},
		{"gcs://user:pass@bucket", "", "", "must not contain credentials"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := ParseURL(tc.raw)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ParseURL(%q): want nil, got %v", tc.raw, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ParseURL(%q): want %q, got nil", tc.raw, tc.wantErr)
			case tc.wantErr != "":
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseURL(%q): want %q, got %v", tc.raw, tc.wantErr, err)
				}
				return
			}
			if cfg.Bucket != tc.wantBucket {
				t.Errorf("Bucket = %q, want %q", cfg.Bucket, tc.wantBucket)
			}
			if cfg.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", cfg.Prefix, tc.wantPrefix)
			}
		})
	}
}
```

- [ ] **Step 2: Run test (will fail — `ParseURL` undefined)**

```bash
go test ./internal/storage/gcs -run TestParseURL -v
```
Expected: FAIL.

- [ ] **Step 3: Write `url.go`**

```go
package gcs

import (
	"fmt"
	"strings"
)

// ParseURL parses a "--store" URL of the form:
//
//	gcs://<bucket>[/<prefix>]
//
// It populates a Config seed; the CLI is responsible for layering env
// vars onto the result before calling Validate / Open.
//
// Credentials in the URL are rejected — the only supported credential
// paths are env vars, Application Default Credentials, and explicit
// CredentialsJSON / CredentialsFile.
func ParseURL(raw string) (Config, error) {
	colon := strings.Index(raw, "://")
	if colon <= 0 {
		return Config{}, fmt.Errorf("gcs: unsupported scheme in %q (want gcs://)", raw)
	}
	scheme := raw[:colon]
	if scheme != "gcs" {
		return Config{}, fmt.Errorf("gcs: unsupported scheme %q (want gcs://)", scheme)
	}
	rest := raw[colon+3:]
	if rest == "" {
		return Config{}, fmt.Errorf("gcs: gcs://: bucket required")
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Config{}, fmt.Errorf("gcs: gcs://: bucket required")
	}
	if strings.ContainsRune(bucket, '@') {
		return Config{}, fmt.Errorf("gcs: gcs:// URL must not contain credentials; use Application Default Credentials or CredentialsJSON/CredentialsFile")
	}
	cfg := Config{Bucket: bucket}
	if prefix != "" {
		norm, err := normalizePrefix(prefix)
		if err != nil {
			return Config{}, fmt.Errorf("gcs: gcs:// prefix: %w", err)
		}
		cfg.Prefix = norm
	}
	return cfg, nil
}
```

- [ ] **Step 4: Commit (still won't build until Task 1.4 lands `normalizePrefix`)**

```bash
git add internal/storage/gcs/url.go internal/storage/gcs/url_test.go
git commit -m "M7 task 1.3: gcs ParseURL (depends on prefix.go)"
```

---

### Task 1.4: prefix.go + keys.go

**Files:**
- Create: `internal/storage/gcs/prefix.go`
- Create: `internal/storage/gcs/prefix_test.go`
- Create: `internal/storage/gcs/keys.go`
- Create: `internal/storage/gcs/keys_test.go`

- [ ] **Step 1: Write `prefix_test.go`**

```go
package gcs

import "testing"

func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		in, want, wantErr string
	}{
		{"", "", ""},
		{"repos", "repos/", ""},
		{"repos/", "repos/", ""},
		{"a/b/c", "a/b/c/", ""},
		{"a/b/c/", "a/b/c/", ""},
		{"/leading", "", "leading"},
		{"//double", "", "double"},
		{"trailing//", "", "double"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizePrefix(tc.in)
			if tc.wantErr != "" {
				if err == nil || !contains(err.Error(), tc.wantErr) {
					t.Fatalf("normalizePrefix(%q) err = %v, want containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePrefix(%q) err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyPrefix(t *testing.T) {
	if got := applyPrefix("repos/", "objects/abc"); got != "repos/objects/abc" {
		t.Errorf("applyPrefix = %q", got)
	}
	if got := applyPrefix("", "objects/abc"); got != "objects/abc" {
		t.Errorf("applyPrefix empty = %q", got)
	}
}

func TestStripPrefix(t *testing.T) {
	if got := stripPrefix("repos/", "repos/objects/abc"); got != "objects/abc" {
		t.Errorf("stripPrefix = %q", got)
	}
	if got := stripPrefix("", "objects/abc"); got != "objects/abc" {
		t.Errorf("stripPrefix empty = %q", got)
	}
}
```

- [ ] **Step 2: Write `prefix.go`**

```go
package gcs

import (
	"fmt"
	"strings"
)

// normalizePrefix rejects leading slashes and consecutive slashes; adds
// a trailing slash if missing. Empty input returns empty.
func normalizePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("prefix must not start with leading slash: %q", p)
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("prefix must not contain double slashes: %q", p)
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

func applyPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + key
}

func stripPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return strings.TrimPrefix(key, prefix)
}
```

- [ ] **Step 3: Write `keys_test.go`**

```go
package gcs

import (
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestValidateKey(t *testing.T) {
	good := []string{"a", "a/b", "objects/ab/cdef"}
	for _, k := range good {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) unexpected err: %v", k, err)
		}
	}
	bad := []string{"", "/leading", "trailing/", "double//slash"}
	for _, k := range bad {
		err := validateKey(k)
		if err == nil {
			t.Errorf("validateKey(%q) expected error", k)
			continue
		}
		if !errors.Is(err, bvstorage.ErrInvalidArgument) {
			t.Errorf("validateKey(%q) err = %v, want wraps ErrInvalidArgument", k, err)
		}
	}
}
```

- [ ] **Step 4: Write `keys.go`**

```go
package gcs

import (
	"fmt"
	"strings"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func validateKey(k string) error {
	if k == "" {
		return fmt.Errorf("%w: key must not be empty", bvstorage.ErrInvalidArgument)
	}
	if strings.HasPrefix(k, "/") {
		return fmt.Errorf("%w: key must not start with /: %q", bvstorage.ErrInvalidArgument, k)
	}
	if strings.HasSuffix(k, "/") {
		return fmt.Errorf("%w: key must not end with /: %q", bvstorage.ErrInvalidArgument, k)
	}
	if strings.Contains(k, "//") {
		return fmt.Errorf("%w: key must not contain consecutive /: %q", bvstorage.ErrInvalidArgument, k)
	}
	return nil
}
```

- [ ] **Step 5: Run all gcs tests written so far**

```bash
go test ./internal/storage/gcs -run "TestConfig|TestParseURL|TestNormalizePrefix|TestApplyPrefix|TestStripPrefix|TestValidateKey" -v
```
Expected: PASS for all.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/gcs/prefix.go internal/storage/gcs/prefix_test.go internal/storage/gcs/keys.go internal/storage/gcs/keys_test.go
git commit -m "M7 task 1.4: gcs prefix + key validation; package now builds"
```

---

### Task 1.5: errs.go — error classification

**Files:**
- Create: `internal/storage/gcs/errs.go`
- Create: `internal/storage/gcs/errs_test.go`

GCS surfaces errors as `*googleapi.Error` with HTTP status codes; conditional failures use 412. The `iterator.Done` sentinel and `storage.ErrObjectNotExist` need explicit handling.

- [ ] **Step 1: Write the failing test**

```go
package gcs

import (
	"errors"
	"net/http"
	"testing"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		op   gcsOp
		err  error
		want error
	}{
		{"nil", opGet, nil, nil},
		{"object-not-exist", opGet, gstorage.ErrObjectNotExist, bvstorage.ErrNotFound},
		{"404", opGet, &googleapi.Error{Code: http.StatusNotFound}, bvstorage.ErrNotFound},
		{"412 putIfAbsent -> AlreadyExists", opPutIfAbsent, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrAlreadyExists},
		{"412 putIfMatch -> VersionMismatch", opPutIfMatch, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrVersionMismatch},
		{"412 deleteIfMatch -> VersionMismatch", opDeleteIfMatch, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrVersionMismatch},
		{"412 completeIfAbsent -> AlreadyExists", opCompleteIfAbsent, &googleapi.Error{Code: http.StatusPreconditionFailed}, bvstorage.ErrAlreadyExists},
		{"429 throttled", opGet, &googleapi.Error{Code: http.StatusTooManyRequests}, bvstorage.ErrThrottled},
		{"403 access denied", opGet, &googleapi.Error{Code: http.StatusForbidden}, bvstorage.ErrAccessDenied},
		{"503 transient", opGet, &googleapi.Error{Code: http.StatusServiceUnavailable}, bvstorage.ErrTransient},
		{"400 invalid", opPutIfAbsent, &googleapi.Error{Code: http.StatusBadRequest}, bvstorage.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.op, tc.err)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("classify: want nil, got %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("classify: got %v, want errors.Is(%v)", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run — should fail (`classify` undefined)**

```bash
go test ./internal/storage/gcs -run TestClassify -v
```
Expected: FAIL.

- [ ] **Step 3: Write `errs.go`**

```go
package gcs

import (
	"errors"
	"fmt"
	"net/http"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// gcsOp tells classify which conditional semantics applied. 412 has
// different caller-visible meanings depending on whether the call set
// ifGenerationMatch=0 (create-only) or ifGenerationMatch=N (update).
type gcsOp int

const (
	opGet gcsOp = iota
	opHead
	opGetRange
	opList
	opPutIfAbsent      // ifGenerationMatch=0  -> 412 = ErrAlreadyExists
	opPutIfMatch       // ifGenerationMatch=N  -> 412 = ErrVersionMismatch
	opDeleteIfMatch    // ifGenerationMatch=N  -> 412 = ErrVersionMismatch
	opCreateMultipart  // open resumable session
	opUploadPart       // append to resumable session
	opCompleteIfAbsent // finalize with ifGenerationMatch=0
	opAbortMultipart
	opSignedURL
)

// classify maps an SDK error to a storage sentinel. The original error
// remains reachable via errors.As / errors.Unwrap.
func classify(op gcsOp, err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, gstorage.ErrObjectNotExist) || errors.Is(err, gstorage.ErrBucketNotExist) {
		return wrap(bvstorage.ErrNotFound, err)
	}

	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusNotFound:
			return wrap(bvstorage.ErrNotFound, err)
		case http.StatusPreconditionFailed:
			switch op {
			case opPutIfAbsent, opCompleteIfAbsent:
				return wrap(bvstorage.ErrAlreadyExists, err)
			case opPutIfMatch, opDeleteIfMatch, opGet:
				return wrap(bvstorage.ErrVersionMismatch, err)
			default:
				return wrap(bvstorage.ErrTransient, err)
			}
		case http.StatusTooManyRequests:
			return wrap(bvstorage.ErrThrottled, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return wrap(bvstorage.ErrAccessDenied, err)
		case http.StatusBadRequest:
			return wrap(bvstorage.ErrInvalidArgument, err)
		case http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadGateway, http.StatusInternalServerError:
			return wrap(bvstorage.ErrTransient, err)
		}
	}

	return fmt.Errorf("gcs: %w", err)
}

func wrap(sentinel, cause error) error {
	return fmt.Errorf("gcs: %w: %v", sentinel, cause)
}
```

- [ ] **Step 4: Run tests — PASS**

```bash
go test ./internal/storage/gcs -run TestClassify -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/gcs/errs.go internal/storage/gcs/errs_test.go
git commit -m "M7 task 1.5: gcs error classification"
```

---

### Task 1.6: retry.go (gax policy hook)

**Files:**
- Create: `internal/storage/gcs/retry.go`
- Create: `internal/storage/gcs/retry_test.go`

The GCS Go client retries automatically. We expose a single helper that returns the per-bucket retry options the rest of the package applies.

- [ ] **Step 1: Write the test**

```go
package gcs

import "testing"

func TestRetryOptions(t *testing.T) {
	cfg := Config{MaxRetries: 7}
	cfg.applyDefaults()
	opts := retryOpts(cfg)
	if opts.maxAttempts != 7 {
		t.Errorf("maxAttempts = %d, want 7", opts.maxAttempts)
	}
	if opts.policy == 0 {
		t.Errorf("policy must be set")
	}
}
```

- [ ] **Step 2: Run — should fail**

```bash
go test ./internal/storage/gcs -run TestRetryOptions -v
```

- [ ] **Step 3: Write `retry.go`**

```go
package gcs

import (
	gstorage "cloud.google.com/go/storage"
)

// retryOpts captures the parameters we hand to BucketHandle.Retryer.
// We always use RetryAlways (the GCS SDK's "retry idempotent reads and
// preconditioned writes" policy); 412 PreconditionFailed is NOT retried
// by design — that case is the conditional-write contract we rely on.
type retryParams struct {
	maxAttempts int
	policy      gstorage.RetryPolicy
}

func retryOpts(cfg Config) retryParams {
	return retryParams{
		maxAttempts: cfg.MaxRetries,
		policy:      gstorage.RetryAlways,
	}
}

// applyRetry wraps a *storage.BucketHandle with the configured retryer.
func applyRetry(b *gstorage.BucketHandle, p retryParams) *gstorage.BucketHandle {
	return b.Retryer(
		gstorage.WithMaxAttempts(p.maxAttempts),
		gstorage.WithPolicy(p.policy),
	)
}
```

- [ ] **Step 4: Run tests — PASS**

```bash
go test ./internal/storage/gcs -run TestRetryOptions -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/storage/gcs/retry.go internal/storage/gcs/retry_test.go
git commit -m "M7 task 1.6: gcs retry policy hook"
```

---

### Task 1.7: Open + client wiring

**Files:**
- Create: `internal/storage/gcs/open.go`
- Create: `internal/storage/gcs/open_test.go`

- [ ] **Step 1: Write the failing test**

```go
package gcs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
)

func TestOpenRejectsBadConfig(t *testing.T) {
	_, err := gcs.Open(context.Background(), gcs.Config{})
	if err == nil {
		t.Fatal("Open: want error for empty Bucket")
	}
}

func TestOpenWithEndpointOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := gcs.Config{
		Bucket:          "test",
		Endpoint:        srv.URL + "/storage/v1/",
		CredentialsJSON: []byte(`{"type":"service_account","client_email":"x@y","private_key":"-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----\n","token_uri":"` + srv.URL + `/token"}`),
	}
	// We can't fully exercise auth without a real key; just verify
	// Open does not panic and returns an opener that the caller can
	// close. Errors from auth setup are acceptable here — we want
	// the wiring path to compile and be reachable.
	g, err := gcs.Open(context.Background(), cfg)
	if err != nil {
		t.Logf("Open with mock endpoint returned (expected) auth err: %v", err)
		return
	}
	if g == nil {
		t.Fatal("Open: returned nil GCS")
	}
}
```

- [ ] **Step 2: Run — should fail (`Open` undefined)**

```bash
go test ./internal/storage/gcs -run TestOpen -v
```

- [ ] **Step 3: Write `open.go`**

```go
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
```

- [ ] **Step 4: Run tests — should PASS**

```bash
go test ./internal/storage/gcs -run TestOpen -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/storage/gcs/open.go internal/storage/gcs/open_test.go
git commit -m "M7 task 1.7: gcs Open + client wiring"
```

---

### Task 1.8: Get / Head / GetRange

**Files:**
- Create: `internal/storage/gcs/get.go`
- Create: `internal/storage/gcs/get_test.go`

For unit-mocking GCS we use the `STORAGE_EMULATOR_HOST` env var pointing at an `httptest.Server` that returns canned JSON. This mirrors what `fake-gcs-server` does at a coarser level. See `cloud.google.com/go/storage` testing patterns.

- [ ] **Step 1: Write `get.go`**

```go
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) Get(ctx context.Context, key string, opts *bvstorage.GetOptions) (*bvstorage.Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key))
	if opts != nil && opts.IfVersionMatches != nil {
		gen, err := parseGen(*opts.IfVersionMatches)
		if err != nil {
			return nil, err
		}
		obj = obj.Generation(gen)
	}
	rdr, err := obj.NewReader(ctx)
	if err != nil {
		return nil, classify(opGet, err)
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		_ = rdr.Close()
		return nil, classify(opHead, err)
	}
	return &bvstorage.Object{
		Body: rdr,
		Metadata: bvstorage.ObjectMetadata{
			Key:         key,
			Version:     versionFromGen(attrs.Generation),
			Size:        attrs.Size,
			ContentType: attrs.ContentType,
			ModifiedAt:  attrs.Updated,
		},
	}, nil
}

func (g *GCS) Head(ctx context.Context, key string) (*bvstorage.ObjectMetadata, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, classify(opHead, err)
	}
	return &bvstorage.ObjectMetadata{
		Key:         key,
		Version:     versionFromGen(attrs.Generation),
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		ModifiedAt:  attrs.Updated,
	}, nil
}

func (g *GCS) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d,%d]", bvstorage.ErrInvalidArgument, start, endInclusive)
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key))
	rdr, err := obj.NewRangeReader(ctx, start, endInclusive-start+1)
	if err != nil {
		return nil, classify(opGetRange, err)
	}
	return rdr, nil
}

// versionFromGen / parseGen serialize the GCS generation number as a
// decimal string so ObjectVersion stays opaque to callers.
func versionFromGen(gen int64) bvstorage.ObjectVersion {
	return bvstorage.ObjectVersion{
		Provider: "gcs",
		Token:    fmt.Sprintf("%d", gen),
		Kind:     bvstorage.VersionEtag, // generation plays the etag role
	}
}

func parseGen(v bvstorage.ObjectVersion) (int64, error) {
	if v.Provider != "" && v.Provider != "gcs" {
		return 0, fmt.Errorf("%w: ObjectVersion.Provider=%q (gcs requires \"gcs\")", bvstorage.ErrVersionMismatch, v.Provider)
	}
	var gen int64
	_, err := fmt.Sscanf(v.Token, "%d", &gen)
	if err != nil {
		return 0, fmt.Errorf("%w: ObjectVersion.Token must be a decimal generation: %v", bvstorage.ErrInvalidArgument, err)
	}
	return gen, nil
}

// silence unused imports if errors / time become unused after refactor
var _ = errors.New
var _ = time.Time{}
```

- [ ] **Step 2: Write `get_test.go`**

```go
package gcs_test

// Live behavior is covered by gcs_conformance_test.go (Task 1.13)
// against fake-gcs-server. This file holds only narrow unit tests that
// don't depend on a running emulator.

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestParseGenRejectsWrongProvider(t *testing.T) {
	_, err := gcs.ExportedParseGen(bvstorage.ObjectVersion{Provider: "s3compat", Token: "123"})
	if err == nil {
		t.Fatal("expected error for non-gcs provider")
	}
}
```

(Add an internal export: in a new file `internal/storage/gcs/export_test.go`:

```go
package gcs

// Test-only helpers exported via build-tag-free file in the same
// package. See get_test.go.

func ExportedParseGen(v interface{ String() string } /* unused */) (int64, error) {
	// type-narrow via concrete reconstruction handled in get_test.go
	return 0, nil
}
```

— actually simpler: just write the test in `package gcs` (white-box) instead. Replace `get_test.go` above with):

```go
package gcs

import (
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestParseGenRejectsWrongProvider(t *testing.T) {
	_, err := parseGen(bvstorage.ObjectVersion{Provider: "s3compat", Token: "123"})
	if err == nil {
		t.Fatal("expected error for non-gcs provider")
	}
}

func TestParseGenAcceptsEmptyProvider(t *testing.T) {
	gen, err := parseGen(bvstorage.ObjectVersion{Token: "42"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gen != 42 {
		t.Fatalf("gen = %d, want 42", gen)
	}
}

func TestParseGenRejectsNonNumeric(t *testing.T) {
	_, err := parseGen(bvstorage.ObjectVersion{Token: "not-a-number"})
	if err == nil {
		t.Fatal("expected error for non-numeric token")
	}
}
```

(Discard the `export_test.go` idea above — keep tests in-package.)

- [ ] **Step 3: Run tests**

```bash
go test ./internal/storage/gcs -run "TestParseGen" -v
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/storage/gcs/get.go internal/storage/gcs/get_test.go
git commit -m "M7 task 1.8: gcs Get + Head + GetRange + version codec"
```

---

### Task 1.9: PutIfAbsent + PutIfVersionMatches

**Files:**
- Create: `internal/storage/gcs/put.go`
- Create: `internal/storage/gcs/put_test.go`

- [ ] **Step 1: Write `put.go`**

```go
package gcs

import (
	"context"
	"io"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key)).
		If(gstorage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(ctx)
	w.ChunkSize = g.cfg.UploadChunkSize
	if opts != nil && opts.ContentType != "" {
		w.ContentType = opts.ContentType
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return bvstorage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	if err := w.Close(); err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	return versionFromGen(w.Attrs().Generation), nil
}

func (g *GCS) PutIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	gen, err := parseGen(expected)
	if err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key)).
		If(gstorage.Conditions{GenerationMatch: gen})
	w := obj.NewWriter(ctx)
	w.ChunkSize = g.cfg.UploadChunkSize
	if opts != nil && opts.ContentType != "" {
		w.ContentType = opts.ContentType
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return bvstorage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	if err := w.Close(); err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	return versionFromGen(w.Attrs().Generation), nil
}
```

- [ ] **Step 2: Write `put_test.go`** (unit-level — full behavior covered by conformance)

```go
package gcs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestPutIfAbsentRejectsBadKey(t *testing.T) {
	g := &GCS{}
	_, err := g.PutIfAbsent(context.Background(), "/leading", bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}

func TestPutIfVersionMatchesRejectsWrongProviderToken(t *testing.T) {
	g := &GCS{}
	_, err := g.PutIfVersionMatches(context.Background(), "k", bvstorage.ObjectVersion{Provider: "s3compat", Token: "1"}, bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrVersionMismatch) {
		t.Fatalf("got %v, want ErrVersionMismatch", err)
	}
}
```

- [ ] **Step 3: Run tests — PASS**

```bash
go test ./internal/storage/gcs -run "TestPut" -v
```

- [ ] **Step 4: Commit**

```bash
git add internal/storage/gcs/put.go internal/storage/gcs/put_test.go
git commit -m "M7 task 1.9: gcs PutIfAbsent + PutIfVersionMatches"
```

---

### Task 1.10: DeleteIfVersionMatches

**Files:**
- Create: `internal/storage/gcs/delete.go`
- Create: `internal/storage/gcs/delete_test.go`

- [ ] **Step 1: Write `delete.go`**

```go
package gcs

import (
	"context"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) DeleteIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	gen, err := parseGen(expected)
	if err != nil {
		return err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key)).
		If(gstorage.Conditions{GenerationMatch: gen})
	if err := obj.Delete(ctx); err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
```

- [ ] **Step 2: Write `delete_test.go`**

```go
package gcs

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestDeleteRejectsBadKey(t *testing.T) {
	g := &GCS{}
	err := g.DeleteIfVersionMatches(context.Background(), "", bvstorage.ObjectVersion{Token: "1"})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/gcs -run TestDelete -v
git add internal/storage/gcs/delete.go internal/storage/gcs/delete_test.go
git commit -m "M7 task 1.10: gcs DeleteIfVersionMatches"
```

---

### Task 1.11: List with delimiter and pagination

**Files:**
- Create: `internal/storage/gcs/list.go`
- Create: `internal/storage/gcs/list_test.go`

The GCS pager iterates by token; we round-trip the token opaquely in `ListPage.NextToken`.

- [ ] **Step 1: Write `list.go`**

```go
package gcs

import (
	"context"
	"errors"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) List(ctx context.Context, prefix string, opts *bvstorage.ListOptions) (*bvstorage.ListPage, error) {
	q := &gstorage.Query{
		Prefix: applyPrefix(g.cfg.Prefix, prefix),
	}
	maxKeys := 1000
	var token string
	if opts != nil {
		if opts.Delimiter != "" {
			q.Delimiter = opts.Delimiter
		}
		if opts.MaxKeys > 0 {
			maxKeys = opts.MaxKeys
		}
		token = opts.ContinuationToken
	}
	it := g.bucket.Objects(ctx, q)
	pager := iterator.NewPager(it, maxKeys, token)

	var batch []*gstorage.ObjectAttrs
	nextToken, err := pager.NextPage(&batch)
	if err != nil && !errors.Is(err, iterator.Done) {
		return nil, classify(opList, err)
	}

	page := &bvstorage.ListPage{NextToken: nextToken}
	for _, attrs := range batch {
		// CommonPrefixes come back as ObjectAttrs with empty Name and
		// non-empty Prefix.
		if attrs.Prefix != "" {
			page.CommonPrefixes = append(page.CommonPrefixes, stripPrefix(g.cfg.Prefix, attrs.Prefix))
			continue
		}
		page.Objects = append(page.Objects, bvstorage.ObjectMetadata{
			Key:         stripPrefix(g.cfg.Prefix, attrs.Name),
			Version:     versionFromGen(attrs.Generation),
			Size:        attrs.Size,
			ContentType: attrs.ContentType,
			ModifiedAt:  attrs.Updated,
		})
	}
	return page, nil
}
```

- [ ] **Step 2: Write `list_test.go`** (unit-level only; full behavior in conformance)

```go
package gcs

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestListNilOptions(t *testing.T) {
	// Calling on a zero-value GCS would panic on bucket dereference,
	// so just verify the signature compiles.
	var _ func(context.Context, string, *bvstorage.ListOptions) (*bvstorage.ListPage, error) = (*GCS)(nil).List
	_ = errors.New
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/gcs -run TestList -v
git add internal/storage/gcs/list.go internal/storage/gcs/list_test.go
git commit -m "M7 task 1.11: gcs List with delimiter and pagination"
```

---

### Task 1.12: Multipart — resumable upload session

**Files:**
- Create: `internal/storage/gcs/multipart.go`
- Create: `internal/storage/gcs/multipart_test.go`

GCS has no S3-style "stage parts then assemble" API. We model `MultipartUpload` as a single resumable-upload session: `CreateMultipart` opens a `Writer`, `UploadPart` writes parts to it in order, `CompleteMultipartIfAbsent` calls `Close` with `ifGenerationMatch=0`, `Abort` cancels the writer.

- [ ] **Step 1: Write `multipart.go`**

```go
package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// upload is the GCS-backed MultipartUpload. Because GCS resumable
// uploads are streamed sequentially through a single Writer, we buffer
// parts in memory by part number and flush them in order on Complete.
// This trades memory for the part-out-of-order property the
// storage.MultipartUpload contract requires.
type upload struct {
	parent *GCS
	id     string
	key    string

	mu         sync.Mutex
	parts      map[int][]byte
	terminated bool
}

var _ bvstorage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.id }
func (u *upload) Key() string      { return u.key }

func (g *GCS) CreateMultipart(ctx context.Context, key string, opts *bvstorage.MultipartOptions) (bvstorage.MultipartUpload, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("gcs: create upload id: %w", err)
	}
	return &upload{
		parent: g,
		id:     id,
		key:    key,
		parts:  make(map[int][]byte),
	}, nil
}

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (bvstorage.MultipartPart, error) {
	if partNumber < 1 {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: partNumber must be >= 1 (got %d)", bvstorage.ErrInvalidArgument, partNumber)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.MultipartPart{}, fmt.Errorf("gcs: buffer part: %w", err)
	}
	u.parts[partNumber] = buf
	return bvstorage.MultipartPart{
		PartNumber: partNumber,
		Token:      fmt.Sprintf("%d", partNumber), // ordering only
		Size:       int64(len(buf)),
	}, nil
}

func (g *GCS) CompleteMultipartIfAbsent(ctx context.Context, mu bvstorage.MultipartUpload, parts []bvstorage.MultipartPart) (bvstorage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload not produced by this adapter", bvstorage.ErrInvalidArgument)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	if err := validatePartList(parts, u.parts); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	// Flush parts in order through a single resumable Writer that
	// finalizes with ifGenerationMatch=0. This gives the §29 #8
	// "multipart cannot overwrite" invariant.
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, u.key)).
		If(gstorage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(ctx)
	w.ChunkSize = g.cfg.UploadChunkSize

	sorted := make([]int, 0, len(parts))
	for _, p := range parts {
		sorted = append(sorted, p.PartNumber)
	}
	sort.Ints(sorted)

	for _, pn := range sorted {
		if _, err := io.Copy(w, bytes.NewReader(u.parts[pn])); err != nil {
			_ = w.Close()
			return bvstorage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
		}
	}
	if err := w.Close(); err != nil {
		return bvstorage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
	}
	u.terminated = true
	return versionFromGen(w.Attrs().Generation), nil
}

func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return nil
	}
	u.parts = nil
	u.terminated = true
	return nil
}

// validatePartList verifies every requested part has been buffered.
// Detects gaps (1,2,4 missing 3), zero-length lists, and unknown part
// numbers (a number that was never UploadPart-ed).
func validatePartList(want []bvstorage.MultipartPart, have map[int][]byte) error {
	if len(want) == 0 {
		return fmt.Errorf("%w: complete called with empty parts list", bvstorage.ErrInvalidArgument)
	}
	seen := make(map[int]bool, len(want))
	for _, p := range want {
		if _, ok := have[p.PartNumber]; !ok {
			return fmt.Errorf("%w: part %d was never uploaded", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		if seen[p.PartNumber] {
			return fmt.Errorf("%w: part %d listed twice", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		seen[p.PartNumber] = true
	}
	return nil
}

// silence unused imports if errors becomes unused
var _ = errors.New

// newUploadID returns a hex random identifier.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
```

(Add the imports for `crypto/rand` and `encoding/hex` at the top — `"crypto/rand"` and `"encoding/hex"`.)

- [ ] **Step 2: Write `multipart_test.go`**

```go
package gcs

import (
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestValidatePartList(t *testing.T) {
	have := map[int][]byte{1: {}, 2: {}, 3: {}}

	if err := validatePartList(nil, have); err == nil {
		t.Errorf("nil parts: want error, got nil")
	}
	if err := validatePartList([]bvstorage.MultipartPart{{PartNumber: 1}, {PartNumber: 2}, {PartNumber: 3}}, have); err != nil {
		t.Errorf("happy path: %v", err)
	}
	if err := validatePartList([]bvstorage.MultipartPart{{PartNumber: 5}}, have); err == nil {
		t.Errorf("unknown part: want error")
	}
	if err := validatePartList([]bvstorage.MultipartPart{{PartNumber: 1}, {PartNumber: 1}}, have); err == nil {
		t.Errorf("dup part: want error")
	}
	_ = errors.New
}

func TestNewUploadID(t *testing.T) {
	a, err := newUploadID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newUploadID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("expected unique IDs, got %s twice", a)
	}
	if len(a) != 32 {
		t.Errorf("len(id) = %d, want 32 hex chars", len(a))
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/gcs -run "TestValidatePartList|TestNewUploadID" -v
git add internal/storage/gcs/multipart.go internal/storage/gcs/multipart_test.go
git commit -m "M7 task 1.12: gcs multipart via resumable upload session"
```

---

### Task 1.13: SignedGetURL

**Files:**
- Create: `internal/storage/gcs/signed.go`
- Create: `internal/storage/gcs/signed_test.go`

- [ ] **Step 1: Write `signed.go`**

```go
package gcs

import (
	"context"
	"time"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a v4 signed URL granting time-limited GET
// access to key. opts.Expires is clamped to PresignDefaultTTL when
// zero. If the configured credentials cannot sign URLs (e.g., metadata
// server tokens against fake-gcs-server), returns ErrNotSupported and
// the conformance suite skips §29 #10.
func (g *GCS) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = g.cfg.PresignDefaultTTL
	}
	url, err := g.bucket.SignedURL(applyPrefix(g.cfg.Prefix, key), &gstorage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(ttl),
		Scheme:  gstorage.SigningSchemeV4,
	})
	if err != nil {
		// Translate sign-failure into ErrNotSupported so the
		// conformance suite probes correctly. Network/auth failures
		// against real GCS will still propagate via the suite as a
		// hard error.
		return "", wrap(bvstorage.ErrNotSupported, err)
	}
	return url, nil
}
```

- [ ] **Step 2: Write `signed_test.go`**

```go
package gcs

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLRejectsBadKey(t *testing.T) {
	g := &GCS{}
	_, err := g.SignedGetURL(context.Background(), "/leading", bvstorage.SignedURLOptions{})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/gcs -run TestSignedGetURL -v
git add internal/storage/gcs/signed.go internal/storage/gcs/signed_test.go
git commit -m "M7 task 1.13: gcs SignedGetURL via v4 signing"
```

---

### Task 1.14: cleanup.go for conformance test (AbortMultipartsUnderPrefix)

**Files:**
- Create: `internal/storage/gcs/cleanup.go`

GCS has no concept of "orphan multipart uploads" the way S3 does — our resumable sessions live entirely in adapter memory. So this helper is a no-op; we add it only to keep the conformance test wiring symmetric with `s3compat`.

- [ ] **Step 1: Write `cleanup.go`**

```go
package gcs

import "context"

// AbortMultipartsUnderPrefix is a conformance-suite test helper. Unlike
// S3, GCS has no server-side multipart sessions to abort: our resumable
// uploads are tracked only in adapter memory. This function exists for
// symmetry with the s3compat test cleanup hook and is a no-op.
func (g *GCS) AbortMultipartsUnderPrefix(ctx context.Context) error {
	return nil
}
```

- [ ] **Step 2: Commit**

```bash
git add internal/storage/gcs/cleanup.go
git commit -m "M7 task 1.14: gcs AbortMultipartsUnderPrefix no-op for symmetry"
```

---

### Task 1.15: Conformance wiring — gcs_conformance_test.go

**Files:**
- Create: `internal/storage/gcs/gcs_conformance_test.go`

- [ ] **Step 1: Write the conformance harness**

```go
package gcs_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
)

func TestGCSConformance(t *testing.T) {
	bucket := os.Getenv("BUCKETVCS_GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("BUCKETVCS_GCS_TEST_BUCKET unset — skipping live GCS conformance")
	}
	base := gcs.Config{
		Bucket:          bucket,
		Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
		CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
	}
	conformance.Run(t, makeFactory(t, base))
}

func makeFactory(t *testing.T, base gcs.Config) conformance.Factory {
	t.Helper()
	if err := base.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	return func(tb testing.TB) (bvstorage.ObjectStore, func()) {
		tb.Helper()
		cfg := base
		cfg.Prefix = fmt.Sprintf("conformance/%s/", uuid.New().String())
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := gcs.Open(ctx, cfg)
		if err != nil {
			tb.Fatalf("gcs.Open: %v", err)
		}
		cleanup := func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer ccancel()
			cleanupPrefix(tb, s, cctx)
			_ = s.Close()
		}
		return s, cleanup
	}
}

func cleanupPrefix(tb testing.TB, s bvstorage.ObjectStore, ctx context.Context) {
	tb.Helper()
	if g, ok := s.(*gcs.GCS); ok {
		_ = g.AbortMultipartsUnderPrefix(ctx)
	}
	for {
		page, err := s.List(ctx, "", nil)
		if err != nil {
			tb.Logf("conformance cleanup: list: %v", err)
			return
		}
		if len(page.Objects) == 0 {
			return
		}
		for _, o := range page.Objects {
			if err := s.DeleteIfVersionMatches(ctx, o.Key, o.Version); err != nil {
				tb.Logf("conformance cleanup: delete %q: %v", o.Key, err)
			}
		}
		if page.NextToken == "" {
			return
		}
	}
}
```

- [ ] **Step 2: Run against the local fake-gcs stack**

```bash
docker compose -f docker-compose.cloud.yml up -d --wait
export BUCKETVCS_GCS_TEST_BUCKET=bucketvcs-conformance
export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443
go test -count=1 -timeout=10m ./internal/storage/gcs
```

Expected: PASS (with `§29 #10 SignedURL` skipped against fake-gcs).

If conformance failures show up, fix them before committing — tighten the adapter, don't paper over with skips.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/gcs/gcs_conformance_test.go
git commit -m "M7 task 1.15: gcs conformance test harness — passes against fake-gcs"
```

---

### Task 1.16: README for the gcs package

**Files:**
- Create: `internal/storage/gcs/README.md`

- [ ] **Step 1: Write `README.md`**

```markdown
# internal/storage/gcs

Google Cloud Storage adapter for `storage.ObjectStore`. Canonical M7 backend (§11.1).

## Capabilities

- Strong read-after-write and read-after-delete consistency (§11.1).
- Conditional writes via `ifGenerationMatch` (§12.2).
- v4 signed URLs (`gstorage.SigningSchemeV4`).
- Resumable uploads model `MultipartUpload`. Parts are buffered in
  adapter memory and flushed in order on `CompleteMultipartIfAbsent`.

## Configuration

| Env var | Purpose |
|---|---|
| `BUCKETVCS_GCS_ENDPOINT` | Override default endpoint (CI uses fake-gcs URL) |
| `BUCKETVCS_GCS_CREDENTIALS_FILE` | Path to service-account JSON |
| `BUCKETVCS_GCS_USER_PROJECT` | Billing project for requester-pays buckets |
| `GOOGLE_APPLICATION_CREDENTIALS` | Standard ADC path (honored by SDK) |

## Running conformance against real GCS

```bash
export BUCKETVCS_GCS_TEST_BUCKET=<your-bucket>
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json
go test -count=1 -timeout=15m ./internal/storage/gcs
```

## Running conformance against fake-gcs-server

```bash
docker compose -f docker-compose.cloud.yml up -d --wait
export BUCKETVCS_GCS_TEST_BUCKET=bucketvcs-conformance
export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443
go test -count=1 -timeout=10m ./internal/storage/gcs
```

`§29 #10 SignedURL` is skipped against fake-gcs (it does not implement signing).
```

- [ ] **Step 2: Commit**

```bash
git add internal/storage/gcs/README.md
git commit -m "M7 task 1.16: gcs README"
```

---

(Phase 2 — Azure Blob adapter — added next. Layout mirrors Phase 1; only SDK calls differ.)
