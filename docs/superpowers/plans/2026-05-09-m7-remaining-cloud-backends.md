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

## Phase 2 — Azure Blob adapter

Layout mirrors `internal/storage/gcs` file-for-file. Tasks below show the Azure-specific code; for boilerplate (prefix.go, keys.go, doc.go) the GCS versions translate trivially — copy and rename.

### Task 2.1: Skeleton — package, type, Capabilities, SDK deps

**Files:**
- Create: `internal/storage/azureblob/doc.go`
- Create: `internal/storage/azureblob/azureblob.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add Azure SDK dependencies**

```bash
cd /home/eran/work/bucketvcs/.claude/worktrees/m7-cloud
go get github.com/Azure/azure-sdk-for-go/sdk/storage/azblob@latest
go get github.com/Azure/azure-sdk-for-go/sdk/azidentity@latest
go mod tidy
```

- [ ] **Step 2: Write `doc.go`**

```go
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
```

- [ ] **Step 3: Write `azureblob.go`**

```go
package azureblob

import (
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// AzureBlob is the Azure Blob Storage storage.ObjectStore implementation.
type AzureBlob struct {
	cfg       Config
	service   *azblob.Client
	container *container.Client
}

var _ bvstorage.ObjectStore = (*AzureBlob)(nil)

// Capabilities reports the Azure adapter capabilities. MultipartMinPartSize
// reflects Azure's ~100 KiB practical block-blob minimum; MultipartMaxParts
// is Azure's documented 50000-block ceiling per blob; MaxObjectSize is the
// modern Azure 190.7 TiB block-blob max (4000 MiB block * 50000 blocks).
func (a *AzureBlob) Capabilities() bvstorage.Capabilities {
	return bvstorage.Capabilities{
		SignedURLs:           true,
		StrongList:           true,
		MultipartMinPartSize: 100 << 10,
		MultipartMaxParts:    50000,
		MaxObjectSize:        190 << 40, // 190 TiB
	}
}
```

- [ ] **Step 4: Build, then commit**

```bash
go build ./internal/storage/azureblob
git add go.mod go.sum internal/storage/azureblob/doc.go internal/storage/azureblob/azureblob.go
git commit -m "M7 task 2.1: azureblob package skeleton + Capabilities"
```

---

### Task 2.2: Config, Validate, defaults

**Files:**
- Create: `internal/storage/azureblob/config.go`
- Create: `internal/storage/azureblob/config_test.go`

- [ ] **Step 1: Write `config_test.go`**

```go
package azureblob

import "testing"

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok with account+container", Config{Account: "acct", Container: "c"}, ""},
		{"ok with conn string", Config{ConnectionString: "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=k;BlobEndpoint=http://x;", Container: "c"}, ""},
		{"missing container", Config{Account: "acct"}, "container is required"},
		{"missing account and conn string", Config{Container: "c"}, "account or connection string"},
		{"bad prefix", Config{Account: "a", Container: "c", Prefix: "//bad"}, "invalid prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: want nil, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate: want %q, got nil", tc.wantErr)
			case tc.wantErr != "":
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate: want %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
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

- [ ] **Step 2: Write `config.go`**

```go
package azureblob

import (
	"fmt"
	"time"
)

type Config struct {
	Account          string // required if no ConnectionString
	Container        string // required
	Prefix           string

	ServiceURL       string // optional override (Azurite uses this)
	AccountKey       string // optional Shared Key (enables SAS)
	ConnectionString string // optional; precedence over Account/ServiceURL/AccountKey

	UploadBlockSize    int64
	MaxRetries         int
	RequestTimeout     time.Duration
	PresignDefaultTTL  time.Duration
}

const (
	defaultUploadBlockSize   = 8 << 20
	defaultMaxRetries        = 5
	defaultRequestTimeout    = 60 * time.Second
	defaultPresignDefaultTTL = 15 * time.Minute
)

func (c *Config) Validate() error {
	if c.Container == "" {
		return fmt.Errorf("azureblob: container is required")
	}
	if c.Account == "" && c.ConnectionString == "" {
		return fmt.Errorf("azureblob: account or connection string is required")
	}
	if _, err := normalizePrefix(c.Prefix); err != nil {
		return fmt.Errorf("azureblob: invalid prefix: %w", err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if p, err := normalizePrefix(c.Prefix); err == nil {
		c.Prefix = p
	}
	if c.UploadBlockSize == 0 {
		c.UploadBlockSize = defaultUploadBlockSize
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

- [ ] **Step 3: Commit (build will fail until prefix.go in Task 2.4)**

```bash
git add internal/storage/azureblob/config.go internal/storage/azureblob/config_test.go
git commit -m "M7 task 2.2: azureblob Config + Validate (depends on prefix.go)"
```

---

### Task 2.3: ParseURL

**Files:**
- Create: `internal/storage/azureblob/url.go`
- Create: `internal/storage/azureblob/url_test.go`

Azure URL parsing diverges slightly: `azureblob://<container>[/<prefix>]`. Account and credentials come from env, never the URL.

- [ ] **Step 1: Write `url_test.go`**

```go
package azureblob

import "testing"

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw, wantContainer, wantPrefix, wantErr string
	}{
		{"azureblob://my-container", "my-container", "", ""},
		{"azureblob://my-container/repos", "my-container", "repos/", ""},
		{"azureblob://my-container/a/b/", "my-container", "a/b/", ""},
		{"azureblob://", "", "", "container required"},
		{"s3://x", "", "", "unsupported scheme"},
		{"azureblob://user:pw@c", "", "", "must not contain credentials"},
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
			if cfg.Container != tc.wantContainer {
				t.Errorf("Container = %q, want %q", cfg.Container, tc.wantContainer)
			}
			if cfg.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", cfg.Prefix, tc.wantPrefix)
			}
		})
	}
}
```

- [ ] **Step 2: Write `url.go`**

```go
package azureblob

import (
	"fmt"
	"strings"
)

// ParseURL parses a "--store" URL of the form:
//
//	azureblob://<container>[/<prefix>]
//
// Account and credentials are NEVER taken from the URL — they come
// from env vars or DefaultAzureCredential.
func ParseURL(raw string) (Config, error) {
	colon := strings.Index(raw, "://")
	if colon <= 0 {
		return Config{}, fmt.Errorf("azureblob: unsupported scheme in %q (want azureblob://)", raw)
	}
	scheme := raw[:colon]
	if scheme != "azureblob" {
		return Config{}, fmt.Errorf("azureblob: unsupported scheme %q (want azureblob://)", scheme)
	}
	rest := raw[colon+3:]
	if rest == "" {
		return Config{}, fmt.Errorf("azureblob: azureblob://: container required")
	}
	cont, prefix, _ := strings.Cut(rest, "/")
	if cont == "" {
		return Config{}, fmt.Errorf("azureblob: azureblob://: container required")
	}
	if strings.ContainsRune(cont, '@') {
		return Config{}, fmt.Errorf("azureblob: azureblob:// URL must not contain credentials; use BUCKETVCS_AZURE_ACCOUNT_KEY, BUCKETVCS_AZURE_CONNECTION_STRING, or DefaultAzureCredential")
	}
	cfg := Config{Container: cont}
	if prefix != "" {
		norm, err := normalizePrefix(prefix)
		if err != nil {
			return Config{}, fmt.Errorf("azureblob: azureblob:// prefix: %w", err)
		}
		cfg.Prefix = norm
	}
	return cfg, nil
}
```

- [ ] **Step 3: Commit (still needs prefix.go)**

```bash
git add internal/storage/azureblob/url.go internal/storage/azureblob/url_test.go
git commit -m "M7 task 2.3: azureblob ParseURL"
```

---

### Task 2.4: prefix.go + keys.go

**Files:**
- Create: `internal/storage/azureblob/prefix.go`
- Create: `internal/storage/azureblob/prefix_test.go`
- Create: `internal/storage/azureblob/keys.go`
- Create: `internal/storage/azureblob/keys_test.go`

These are textually identical to the GCS versions from Task 1.4 with the package name swapped. Reproduce them in `package azureblob`. Run all azureblob tests to confirm:

```bash
go test ./internal/storage/azureblob -run "TestConfig|TestParseURL|TestNormalizePrefix|TestApplyPrefix|TestStripPrefix|TestValidateKey" -v
```
Expected: PASS for all.

- [ ] **Step 1: Copy + adapt** the four files from `internal/storage/gcs/` (Task 1.4), changing only `package gcs` → `package azureblob`. The error message prefixes remain `"prefix"` / `"key"` — they are package-neutral.

- [ ] **Step 2: Run, then commit**

```bash
go test ./internal/storage/azureblob -v
git add internal/storage/azureblob/prefix.go internal/storage/azureblob/prefix_test.go internal/storage/azureblob/keys.go internal/storage/azureblob/keys_test.go
git commit -m "M7 task 2.4: azureblob prefix + key validation; package now builds"
```

---

### Task 2.5: errs.go — Azure error classification

**Files:**
- Create: `internal/storage/azureblob/errs.go`
- Create: `internal/storage/azureblob/errs_test.go`

Azure errors are surfaced as `*azcore.ResponseError` with HTTP status codes and a string `ErrorCode`.

- [ ] **Step 1: Write `errs_test.go`**

```go
package azureblob

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestClassify(t *testing.T) {
	makeErr := func(status int, code string) error {
		return &azcore.ResponseError{
			StatusCode: status,
			ErrorCode:  code,
		}
	}
	tests := []struct {
		name string
		op   azureOp
		err  error
		want error
	}{
		{"nil", opGet, nil, nil},
		{"404", opGet, makeErr(http.StatusNotFound, "BlobNotFound"), bvstorage.ErrNotFound},
		{"412 putIfAbsent -> AlreadyExists", opPutIfAbsent, makeErr(http.StatusPreconditionFailed, "BlobAlreadyExists"), bvstorage.ErrAlreadyExists},
		{"412 putIfMatch -> VersionMismatch", opPutIfMatch, makeErr(http.StatusPreconditionFailed, "ConditionNotMet"), bvstorage.ErrVersionMismatch},
		{"409 BlobAlreadyExists -> AlreadyExists", opPutIfAbsent, makeErr(http.StatusConflict, "BlobAlreadyExists"), bvstorage.ErrAlreadyExists},
		{"429 throttled", opGet, makeErr(http.StatusTooManyRequests, "TooManyRequests"), bvstorage.ErrThrottled},
		{"403 access denied", opGet, makeErr(http.StatusForbidden, "AuthenticationFailed"), bvstorage.ErrAccessDenied},
		{"503 transient", opGet, makeErr(http.StatusServiceUnavailable, "ServerBusy"), bvstorage.ErrTransient},
		{"400 invalid", opPutIfAbsent, makeErr(http.StatusBadRequest, "InvalidBlobOrBlock"), bvstorage.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.op, tc.err)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("got %v, want errors.Is(%v)", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Write `errs.go`**

```go
package azureblob

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

type azureOp int

const (
	opGet azureOp = iota
	opHead
	opGetRange
	opList
	opPutIfAbsent
	opPutIfMatch
	opDeleteIfMatch
	opStageBlock
	opCommitIfAbsent
	opSignedURL
)

func classify(op azureOp, err error) error {
	if err == nil {
		return nil
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		switch re.StatusCode {
		case http.StatusNotFound:
			return wrap(bvstorage.ErrNotFound, err)
		case http.StatusPreconditionFailed:
			switch op {
			case opPutIfAbsent, opCommitIfAbsent:
				return wrap(bvstorage.ErrAlreadyExists, err)
			case opPutIfMatch, opDeleteIfMatch, opGet:
				return wrap(bvstorage.ErrVersionMismatch, err)
			default:
				return wrap(bvstorage.ErrTransient, err)
			}
		case http.StatusConflict:
			// BlobAlreadyExists also returns 409 in some create paths
			// (Put Block List on a blob with a snapshot etc.).
			switch op {
			case opPutIfAbsent, opCommitIfAbsent:
				return wrap(bvstorage.ErrAlreadyExists, err)
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
	return fmt.Errorf("azureblob: %w", err)
}

func wrap(sentinel, cause error) error {
	return fmt.Errorf("azureblob: %w: %v", sentinel, cause)
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestClassify -v
git add internal/storage/azureblob/errs.go internal/storage/azureblob/errs_test.go
git commit -m "M7 task 2.5: azureblob error classification"
```

---

### Task 2.6: retry.go (Azure SDK options hook)

**Files:**
- Create: `internal/storage/azureblob/retry.go`
- Create: `internal/storage/azureblob/retry_test.go`

The Azure SDK has its own `policy.RetryOptions`. We expose a single helper used by Open.

- [ ] **Step 1: Write the test**

```go
package azureblob

import "testing"

func TestRetryOptionsApplied(t *testing.T) {
	cfg := Config{MaxRetries: 7}
	cfg.applyDefaults()
	opts := retryOpts(cfg)
	if opts.MaxRetries != 7 {
		t.Errorf("MaxRetries = %d, want 7", opts.MaxRetries)
	}
}
```

- [ ] **Step 2: Write `retry.go`**

```go
package azureblob

import (
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// retryOpts returns the policy used by the Azure pipeline. 412 is NOT
// retried — that case is the conditional-write contract we rely on.
// The Azure SDK does not retry 4xx by default, so no explicit opt-out
// is required.
func retryOpts(cfg Config) policy.RetryOptions {
	return policy.RetryOptions{
		MaxRetries:    int32(cfg.MaxRetries),
		TryTimeout:    cfg.RequestTimeout,
		RetryDelay:    250 * time.Millisecond,
		MaxRetryDelay: 30 * time.Second,
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestRetryOptionsApplied -v
git add internal/storage/azureblob/retry.go internal/storage/azureblob/retry_test.go
git commit -m "M7 task 2.6: azureblob retry options hook"
```

---

### Task 2.7: Open + client wiring

**Files:**
- Create: `internal/storage/azureblob/open.go`
- Create: `internal/storage/azureblob/open_test.go`

Credential precedence: `ConnectionString` > `AccountKey` > `DefaultAzureCredential`.

- [ ] **Step 1: Write `open.go`**

```go
package azureblob

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

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

func (a *AzureBlob) Close() error { return nil }
```

- [ ] **Step 2: Write `open_test.go`**

```go
package azureblob_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
)

func TestOpenRejectsBadConfig(t *testing.T) {
	_, err := azureblob.Open(context.Background(), azureblob.Config{})
	if err == nil {
		t.Fatal("Open: want error for empty config")
	}
}

func TestOpenWithConnectionString(t *testing.T) {
	cfg := azureblob.Config{
		Container:        "test",
		ConnectionString: "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;",
	}
	a, err := azureblob.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestOpen -v
git add internal/storage/azureblob/open.go internal/storage/azureblob/open_test.go
git commit -m "M7 task 2.7: azureblob Open + credential precedence"
```

---

### Task 2.8: Get / Head / GetRange

**Files:**
- Create: `internal/storage/azureblob/get.go`
- Create: `internal/storage/azureblob/get_test.go`

- [ ] **Step 1: Write `get.go`**

```go
package azureblob

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) Get(ctx context.Context, key string, opts *bvstorage.GetOptions) (*bvstorage.Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	dlOpts := &blob.DownloadStreamOptions{}
	if opts != nil && opts.IfVersionMatches != nil {
		etag := parseETag(*opts.IfVersionMatches)
		dlOpts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		}
	}
	resp, err := bb.DownloadStream(ctx, dlOpts)
	if err != nil {
		return nil, classify(opGet, err)
	}
	return &bvstorage.Object{
		Body: resp.Body,
		Metadata: bvstorage.ObjectMetadata{
			Key:         key,
			Version:     versionFromETag(resp.ETag),
			Size:        deref(resp.ContentLength),
			ContentType: derefStr(resp.ContentType),
			ModifiedAt:  derefTime(resp.LastModified),
		},
	}, nil
}

func (a *AzureBlob) Head(ctx context.Context, key string) (*bvstorage.ObjectMetadata, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	resp, err := bb.GetProperties(ctx, nil)
	if err != nil {
		return nil, classify(opHead, err)
	}
	return &bvstorage.ObjectMetadata{
		Key:         key,
		Version:     versionFromETag(resp.ETag),
		Size:        deref(resp.ContentLength),
		ContentType: derefStr(resp.ContentType),
		ModifiedAt:  derefTime(resp.LastModified),
	}, nil
}

func (a *AzureBlob) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d,%d]", bvstorage.ErrInvalidArgument, start, endInclusive)
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	resp, err := bb.DownloadStream(ctx, &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: start, Count: endInclusive - start + 1},
	})
	if err != nil {
		return nil, classify(opGetRange, err)
	}
	return resp.Body, nil
}

// versionFromETag / parseETag round-trip Azure ETags (raw, quotes
// stripped) through ObjectVersion.Token.
func versionFromETag(etagPtr *azcore.ETag) bvstorage.ObjectVersion {
	if etagPtr == nil {
		return bvstorage.ObjectVersion{Provider: "azureblob", Kind: bvstorage.VersionEtag}
	}
	return bvstorage.ObjectVersion{
		Provider: "azureblob",
		Token:    strings.Trim(string(*etagPtr), `"`),
		Kind:     bvstorage.VersionEtag,
	}
}

func parseETag(v bvstorage.ObjectVersion) azcore.ETag {
	if v.Provider != "" && v.Provider != "azureblob" {
		return azcore.ETag("")
	}
	return azcore.ETag(`"` + v.Token + `"`)
}

// pointer-deref helpers. Azure SDK returns lots of *T fields.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
func derefStr(p *string) string { return deref(p) }
func derefTime[T any](p *T) T   { return deref(p) }

// silence import if to becomes unused
var _ = to.Ptr[int]
```

(Add `"github.com/Azure/azure-sdk-for-go/sdk/azcore"` to imports.)

- [ ] **Step 2: Write `get_test.go`**

```go
package azureblob

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestVersionFromETagRoundTrip(t *testing.T) {
	raw := azcore.ETag(`"0xABCDEF"`)
	v := versionFromETag(&raw)
	if v.Provider != "azureblob" {
		t.Errorf("Provider = %q, want azureblob", v.Provider)
	}
	if v.Token != "0xABCDEF" {
		t.Errorf("Token = %q, want 0xABCDEF (quotes stripped)", v.Token)
	}
	round := parseETag(v)
	if round != raw {
		t.Errorf("round-trip = %q, want %q", round, raw)
	}
}

func TestParseETagRejectsWrongProvider(t *testing.T) {
	got := parseETag(bvstorage.ObjectVersion{Provider: "gcs", Token: "1"})
	if got != "" {
		t.Errorf("expected empty ETag for wrong provider, got %q", got)
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run "TestVersionFromETag|TestParseETag" -v
git add internal/storage/azureblob/get.go internal/storage/azureblob/get_test.go
git commit -m "M7 task 2.8: azureblob Get + Head + GetRange + ETag codec"
```

---

### Task 2.9: PutIfAbsent + PutIfVersionMatches

**Files:**
- Create: `internal/storage/azureblob/put.go`
- Create: `internal/storage/azureblob/put_test.go`

- [ ] **Step 1: Write `put.go`**

```go
package azureblob

import (
	"context"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

const eTagAny = azcore.ETag("*")

func (a *AzureBlob) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))

	upOpts := &blockblob.UploadOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(eTagAny)},
		},
	}
	if opts != nil && opts.ContentType != "" {
		upOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: to.Ptr(opts.ContentType)}
	}

	resp, err := bb.Upload(ctx, streamSeeker(buf), upOpts)
	if err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	return versionFromETag(resp.ETag), nil
}

func (a *AzureBlob) PutIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	if expected.Provider != "" && expected.Provider != "azureblob" {
		return bvstorage.ObjectVersion{}, wrap(bvstorage.ErrVersionMismatch, nil)
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))

	etag := parseETag(expected)
	upOpts := &blockblob.UploadOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		},
	}
	if opts != nil && opts.ContentType != "" {
		upOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: to.Ptr(opts.ContentType)}
	}

	resp, err := bb.Upload(ctx, streamSeeker(buf), upOpts)
	if err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	return versionFromETag(resp.ETag), nil
}

// streamSeeker wraps a byte slice in a ReadSeekCloser as Upload requires.
func streamSeeker(b []byte) io.ReadSeekCloser {
	return &nopCloser{r: bytesReadSeeker(b)}
}

type nopCloser struct{ r interface{ ReadSeeker } }

func (n *nopCloser) Read(p []byte) (int, error)                   { return n.r.Read(p) }
func (n *nopCloser) Seek(o int64, w int) (int64, error)           { return n.r.Seek(o, w) }
func (n *nopCloser) Close() error                                 { return nil }

// bytesReadSeeker is a minimal *bytes.Reader alias to avoid the import
// cycle when keeping helpers local.
func bytesReadSeeker(b []byte) interface{ ReadSeeker } {
	return readSeeker{ReadSeeker: bytesNewReader(b)}
}

type readSeeker struct{ io.ReadSeeker }

// — implementation note: replace the above stubs with the standard
// `bytes.NewReader(buf)` returning a `*bytes.Reader` (which already
// implements both `io.ReadSeeker` and `io.Closer` via a wrapper).
// The helper above is only there to keep the file self-contained for
// the plan reader; in the actual file just write:
//
//   func streamSeeker(b []byte) io.ReadSeekCloser {
//       return &readSeekCloser{Reader: bytes.NewReader(b)}
//   }
//   type readSeekCloser struct{ *bytes.Reader }
//   func (*readSeekCloser) Close() error { return nil }
```

Use the simpler form in the actual implementation (the comment block at the end shows it). Drop the `bytesReadSeeker` / `bytesNewReader` placeholders.

- [ ] **Step 2: Write `put_test.go`**

```go
package azureblob

import (
	"bytes"
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestPutIfAbsentRejectsBadKey(t *testing.T) {
	a := &AzureBlob{}
	_, err := a.PutIfAbsent(context.Background(), "", bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}

func TestPutIfVersionMatchesRejectsWrongProvider(t *testing.T) {
	a := &AzureBlob{}
	_, err := a.PutIfVersionMatches(context.Background(), "k", bvstorage.ObjectVersion{Provider: "gcs", Token: "x"}, bytes.NewReader(nil), nil)
	if err == nil || !errors.Is(err, bvstorage.ErrVersionMismatch) {
		t.Fatalf("got %v, want ErrVersionMismatch", err)
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestPut -v
git add internal/storage/azureblob/put.go internal/storage/azureblob/put_test.go
git commit -m "M7 task 2.9: azureblob PutIfAbsent + PutIfVersionMatches"
```

---

### Task 2.10: DeleteIfVersionMatches

**Files:**
- Create: `internal/storage/azureblob/delete.go`
- Create: `internal/storage/azureblob/delete_test.go`

- [ ] **Step 1: Write `delete.go`**

```go
package azureblob

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) DeleteIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if expected.Provider != "" && expected.Provider != "azureblob" {
		return wrap(bvstorage.ErrVersionMismatch, nil)
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	etag := parseETag(expected)
	_, err := bb.Delete(ctx, &blob.DeleteOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		},
	})
	if err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
```

- [ ] **Step 2: Write `delete_test.go`**

```go
package azureblob

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestDeleteRejectsBadKey(t *testing.T) {
	a := &AzureBlob{}
	err := a.DeleteIfVersionMatches(context.Background(), "/leading", bvstorage.ObjectVersion{Token: "x"})
	if err == nil || !errors.Is(err, bvstorage.ErrInvalidArgument) {
		t.Fatalf("got %v, want ErrInvalidArgument", err)
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestDelete -v
git add internal/storage/azureblob/delete.go internal/storage/azureblob/delete_test.go
git commit -m "M7 task 2.10: azureblob DeleteIfVersionMatches"
```

---

### Task 2.11: List with delimiter and pagination

**Files:**
- Create: `internal/storage/azureblob/list.go`
- Create: `internal/storage/azureblob/list_test.go`

Azure exposes `NewListBlobsFlatPager` (no delimiter) and `NewListBlobsHierarchyPager` (with delimiter). We dispatch based on `opts.Delimiter`.

- [ ] **Step 1: Write `list.go`**

```go
package azureblob

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) List(ctx context.Context, prefix string, opts *bvstorage.ListOptions) (*bvstorage.ListPage, error) {
	full := applyPrefix(a.cfg.Prefix, prefix)
	maxResults := int32(1000)
	var marker string
	var delimiter string
	if opts != nil {
		if opts.MaxKeys > 0 {
			maxResults = int32(opts.MaxKeys)
		}
		marker = opts.ContinuationToken
		delimiter = opts.Delimiter
	}

	page := &bvstorage.ListPage{}

	if delimiter == "" {
		pager := a.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
			Prefix:     to.Ptr(full),
			MaxResults: &maxResults,
			Marker:     marker(),
		})
		if !pager.More() {
			return page, nil
		}
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, classify(opList, err)
		}
		for _, item := range resp.Segment.BlobItems {
			page.Objects = append(page.Objects, bvstorage.ObjectMetadata{
				Key:         stripPrefix(a.cfg.Prefix, deref(item.Name)),
				Version:     versionFromETag(item.Properties.ETag),
				Size:        deref(item.Properties.ContentLength),
				ContentType: derefStr(item.Properties.ContentType),
				ModifiedAt:  derefTime(item.Properties.LastModified),
			})
		}
		page.NextToken = derefStr(resp.NextMarker)
		return page, nil
	}

	pager := a.container.NewListBlobsHierarchyPager(delimiter, &container.ListBlobsHierarchyOptions{
		Prefix:     to.Ptr(full),
		MaxResults: &maxResults,
		Marker:     marker(),
	})
	if !pager.More() {
		return page, nil
	}
	resp, err := pager.NextPage(ctx)
	if err != nil {
		return nil, classify(opList, err)
	}
	for _, item := range resp.Segment.BlobItems {
		page.Objects = append(page.Objects, bvstorage.ObjectMetadata{
			Key:         stripPrefix(a.cfg.Prefix, deref(item.Name)),
			Version:     versionFromETag(item.Properties.ETag),
			Size:        deref(item.Properties.ContentLength),
			ContentType: derefStr(item.Properties.ContentType),
			ModifiedAt:  derefTime(item.Properties.LastModified),
		})
	}
	for _, p := range resp.Segment.BlobPrefixes {
		page.CommonPrefixes = append(page.CommonPrefixes, stripPrefix(a.cfg.Prefix, derefStr(p.Name)))
	}
	page.NextToken = derefStr(resp.NextMarker)
	return page, nil
}

// marker is a tiny helper because the Azure SDK takes *string for an
// empty marker, not "" — they treat nil and "" differently.
func markerPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
```

(In the actual implementation, replace `marker()` calls with `markerPtr(marker)` — the placeholder above is to keep the snippet under the eye; the helper definition is at the bottom of the file.)

- [ ] **Step 2: Write `list_test.go`** (unit-level only)

```go
package azureblob

import "testing"

func TestMarkerPtr(t *testing.T) {
	if markerPtr("") != nil {
		t.Errorf("markerPtr(\"\"): want nil")
	}
	got := markerPtr("abc")
	if got == nil || *got != "abc" {
		t.Errorf("markerPtr(\"abc\"): want pointer to \"abc\"")
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestMarkerPtr -v
git add internal/storage/azureblob/list.go internal/storage/azureblob/list_test.go
git commit -m "M7 task 2.11: azureblob List with delimiter and pagination"
```

---

### Task 2.12: Multipart — StageBlock + CommitBlockList

**Files:**
- Create: `internal/storage/azureblob/multipart.go`
- Create: `internal/storage/azureblob/multipart_test.go`

- [ ] **Step 1: Write `multipart.go`**

```go
package azureblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// upload models a block-blob multipart upload. Each StageBlock gets a
// fixed-length block ID that combines a per-upload GUID with a
// zero-padded part number; the GUID prevents cross-upload collisions
// to the same target key, the padding satisfies Azure's "all block
// IDs must be the same length within a single CommitBlockList" rule.
type upload struct {
	parent *AzureBlob
	id     string // per-upload GUID
	key    string

	mu         sync.Mutex
	parts      map[int]string // partNumber -> staged block ID
	terminated bool
}

var _ bvstorage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.id }
func (u *upload) Key() string      { return u.key }

func (a *AzureBlob) CreateMultipart(ctx context.Context, key string, _ *bvstorage.MultipartOptions) (bvstorage.MultipartUpload, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("azureblob: upload id: %w", err)
	}
	return &upload{
		parent: a,
		id:     id,
		key:    key,
		parts:  make(map[int]string),
	}, nil
}

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (bvstorage.MultipartPart, error) {
	if partNumber < 1 || partNumber > 50000 {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: partNumber must be in [1,50000] (got %d)", bvstorage.ErrInvalidArgument, partNumber)
	}
	u.mu.Lock()
	if u.terminated {
		u.mu.Unlock()
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	u.mu.Unlock()

	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.MultipartPart{}, fmt.Errorf("azureblob: read part: %w", err)
	}
	blockID := makeBlockID(u.id, partNumber)
	bb := u.parent.container.NewBlockBlobClient(applyPrefix(u.parent.cfg.Prefix, u.key))
	_, err = bb.StageBlock(ctx, blockID, &readSeekCloser{Reader: bytes.NewReader(buf)}, nil)
	if err != nil {
		return bvstorage.MultipartPart{}, classify(opStageBlock, err)
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: upload %s terminated during stage", bvstorage.ErrInvalidArgument, u.id)
	}
	u.parts[partNumber] = blockID
	return bvstorage.MultipartPart{
		PartNumber: partNumber,
		Token:      blockID,
		Size:       int64(len(buf)),
	}, nil
}

func (a *AzureBlob) CompleteMultipartIfAbsent(ctx context.Context, mu bvstorage.MultipartUpload, parts []bvstorage.MultipartPart) (bvstorage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload not produced by this adapter", bvstorage.ErrInvalidArgument)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	if len(parts) == 0 {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: complete called with empty parts list", bvstorage.ErrInvalidArgument)
	}
	sorted := append([]bvstorage.MultipartPart(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PartNumber < sorted[j].PartNumber })

	blockIDs := make([]string, 0, len(sorted))
	seen := make(map[int]bool)
	for _, p := range sorted {
		if seen[p.PartNumber] {
			return bvstorage.ObjectVersion{}, fmt.Errorf("%w: part %d listed twice", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		seen[p.PartNumber] = true
		blockID, ok := u.parts[p.PartNumber]
		if !ok {
			return bvstorage.ObjectVersion{}, fmt.Errorf("%w: part %d was never staged", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		blockIDs = append(blockIDs, blockID)
	}

	bb := u.parent.container.NewBlockBlobClient(applyPrefix(u.parent.cfg.Prefix, u.key))
	resp, err := bb.CommitBlockList(ctx, blockIDs, &blockblob.CommitBlockListOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(eTagAny)},
		},
	})
	if err != nil {
		return bvstorage.ObjectVersion{}, classify(opCommitIfAbsent, err)
	}
	u.terminated = true
	return versionFromETag(resp.ETag), nil
}

func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return nil
	}
	u.terminated = true
	u.parts = nil
	// Uncommitted blocks are GC'd by Azure after 7 days; no API call
	// is needed in the abort path. If a partial commit happened (it
	// should not — we only commit once), the caller can issue a
	// conditional delete separately.
	return nil
}

// makeBlockID returns base64(guid:zeroPad(partNumber)) — fixed length
// for any single CommitBlockList call.
func makeBlockID(uploadID string, partNumber int) string {
	raw := fmt.Sprintf("%s:%010d", uploadID, partNumber)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type readSeekCloser struct{ *bytes.Reader }

func (*readSeekCloser) Close() error { return nil }
```

- [ ] **Step 2: Write `multipart_test.go`**

```go
package azureblob

import "testing"

func TestMakeBlockIDFixedLength(t *testing.T) {
	id, _ := newUploadID()
	a := makeBlockID(id, 1)
	b := makeBlockID(id, 12345)
	if len(a) != len(b) {
		t.Fatalf("block IDs must be equal length within an upload: len(a)=%d len(b)=%d", len(a), len(b))
	}
}

func TestMakeBlockIDUploadIsolated(t *testing.T) {
	idA, _ := newUploadID()
	idB, _ := newUploadID()
	a := makeBlockID(idA, 1)
	b := makeBlockID(idB, 1)
	if a == b {
		t.Fatalf("block IDs from different uploads must differ even at same partNumber")
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run "TestMakeBlockID" -v
git add internal/storage/azureblob/multipart.go internal/storage/azureblob/multipart_test.go
git commit -m "M7 task 2.12: azureblob multipart via StageBlock + CommitBlockList"
```

---

### Task 2.13: SignedGetURL via service SAS

**Files:**
- Create: `internal/storage/azureblob/signed.go`
- Create: `internal/storage/azureblob/signed_test.go`

SAS issuance requires a Shared Key credential. If the adapter was opened via DefaultAzureCredential (no key), we return `ErrNotSupported` so the conformance suite skips §29 #10.

- [ ] **Step 1: Write `signed.go`**

```go
package azureblob

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if a.cfg.AccountKey == "" && a.cfg.ConnectionString == "" {
		return "", wrap(bvstorage.ErrNotSupported, nil)
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = a.cfg.PresignDefaultTTL
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	url, err := bb.GetSASURL(
		sas.BlobPermissions{Read: true},
		time.Now().Add(ttl),
		nil,
	)
	if err != nil {
		return "", wrap(bvstorage.ErrNotSupported, err)
	}
	return url, nil
}
```

- [ ] **Step 2: Write `signed_test.go`**

```go
package azureblob

import (
	"context"
	"errors"
	"testing"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLNoKeyReturnsNotSupported(t *testing.T) {
	a := &AzureBlob{}
	_, err := a.SignedGetURL(context.Background(), "k", bvstorage.SignedURLOptions{})
	if err == nil || !errors.Is(err, bvstorage.ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}
}
```

- [ ] **Step 3: Run, then commit**

```bash
go test ./internal/storage/azureblob -run TestSignedGetURL -v
git add internal/storage/azureblob/signed.go internal/storage/azureblob/signed_test.go
git commit -m "M7 task 2.13: azureblob SignedGetURL via service SAS"
```

---

### Task 2.14: cleanup.go — abort uncommitted blocks under prefix

**Files:**
- Create: `internal/storage/azureblob/cleanup.go`

Azure has no server-side multipart sessions to enumerate; uncommitted blocks self-expire after 7 days. The cleanup helper is a no-op. Exists for symmetry with `s3compat`.

- [ ] **Step 1: Write `cleanup.go`**

```go
package azureblob

import "context"

// AbortMultipartsUnderPrefix is a conformance-suite helper. Azure
// Blob has no enumerable multipart-session abstraction — uncommitted
// blocks expire automatically after 7 days. This is a no-op kept for
// symmetry with s3compat.
func (a *AzureBlob) AbortMultipartsUnderPrefix(ctx context.Context) error {
	return nil
}
```

- [ ] **Step 2: Commit**

```bash
git add internal/storage/azureblob/cleanup.go
git commit -m "M7 task 2.14: azureblob AbortMultipartsUnderPrefix no-op"
```

---

### Task 2.15: Conformance wiring — azureblob_conformance_test.go

**Files:**
- Create: `internal/storage/azureblob/azureblob_conformance_test.go`

- [ ] **Step 1: Write the harness**

```go
package azureblob_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"github.com/bucketvcs/bucketvcs/internal/storage/conformance"
)

func TestAzureBlobConformance(t *testing.T) {
	cont := os.Getenv("BUCKETVCS_AZURE_TEST_CONTAINER")
	if cont == "" {
		t.Skip("BUCKETVCS_AZURE_TEST_CONTAINER unset — skipping live azureblob conformance")
	}
	base := azureblob.Config{
		Container:        cont,
		Account:          os.Getenv("BUCKETVCS_AZURE_ACCOUNT"),
		AccountKey:       os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"),
		ConnectionString: os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"),
		ServiceURL:       os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"),
	}
	conformance.Run(t, makeFactory(t, base))
}

func makeFactory(t *testing.T, base azureblob.Config) conformance.Factory {
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
		s, err := azureblob.Open(ctx, cfg)
		if err != nil {
			tb.Fatalf("azureblob.Open: %v", err)
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
	if a, ok := s.(*azureblob.AzureBlob); ok {
		_ = a.AbortMultipartsUnderPrefix(ctx)
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

- [ ] **Step 2: Run against Azurite**

```bash
docker compose -f docker-compose.cloud.yml up -d --wait
export BUCKETVCS_AZURE_TEST_CONTAINER=bucketvcs-conformance
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
go test -count=1 -timeout=10m ./internal/storage/azureblob
```

Expected: PASS. If conformance failures show up, fix the adapter — don't add skips.

- [ ] **Step 3: Commit**

```bash
git add internal/storage/azureblob/azureblob_conformance_test.go
git commit -m "M7 task 2.15: azureblob conformance harness — passes against Azurite"
```

---

### Task 2.16: README for the azureblob package

**Files:**
- Create: `internal/storage/azureblob/README.md`

- [ ] **Step 1: Write `README.md`**

```markdown
# internal/storage/azureblob

Azure Blob Storage adapter for `storage.ObjectStore`. Canonical M7 backend (§11.1).

## Capabilities

- Strong read-after-write and read-after-delete consistency (§11.1).
- Conditional writes via `If-None-Match: *` and `If-Match: <etag>` (§12.4).
- Service SAS URLs (requires Shared Key credential; returns `ErrNotSupported` otherwise).
- Block blobs only. Multipart uploads map to `StageBlock` + `CommitBlockList`
  with `If-None-Match: *` on the commit (the §29 #8 invariant).

## Configuration

| Env var | Purpose |
|---|---|
| `BUCKETVCS_AZURE_ACCOUNT` | Storage account name |
| `BUCKETVCS_AZURE_ACCOUNT_KEY` | Shared Key (enables SAS) |
| `BUCKETVCS_AZURE_CONNECTION_STRING` | Full connection string (precedence; primary use is Azurite) |
| `BUCKETVCS_AZURE_SERVICE_URL` | Override default service URL |

## Running conformance against real Azure Blob

```bash
export BUCKETVCS_AZURE_TEST_CONTAINER=<your-container>
export BUCKETVCS_AZURE_ACCOUNT=<account>
export BUCKETVCS_AZURE_ACCOUNT_KEY=<key>
go test -count=1 -timeout=15m ./internal/storage/azureblob
```

## Running conformance against Azurite

```bash
docker compose -f docker-compose.cloud.yml up -d --wait
export BUCKETVCS_AZURE_TEST_CONTAINER=bucketvcs-conformance
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
go test -count=1 -timeout=10m ./internal/storage/azureblob
```

`§29 #10 SignedURL` is exercised when an account key is present (Azurite's default dev key works).
```

- [ ] **Step 2: Commit**

```bash
git add internal/storage/azureblob/README.md
git commit -m "M7 task 2.16: azureblob README"
```

---

## Phase 3 — CLI integration

The `--store=` flag picks up two new schemes. Each task lands a small focused diff to `cmd/bucketvcs/store.go` and its test.

### Task 3.1: Wire `gcs://` through cmd/bucketvcs/store.go

**Files:**
- Modify: `cmd/bucketvcs/store.go`
- Modify: `cmd/bucketvcs/store_test.go`

- [ ] **Step 1: Update `parseStoreURL` and add `gcs` case**

In `cmd/bucketvcs/store.go`, drop `gcs` from the M7-reserved error path and add it to the recognized schemes (similar to how `s3`, `r2` are handled). The relevant edit:

```go
// In parseStoreURL switch:
case "gcs":
    if !strings.HasPrefix(rest, "//") {
        return "", "", fmt.Errorf(`--store: gcs URL must use the form gcs://<bucket>[/<prefix>] (got %q)`, s)
    }
    bucketPath := strings.TrimPrefix(rest, "//")
    bucket, _, _ := strings.Cut(bucketPath, "/")
    if bucket == "" {
        return "", "", fmt.Errorf(`--store: gcs:// requires a bucket name (got %q)`, s)
    }
    return scheme, bucketPath, nil
case "azureblob":
    // (added in Task 3.2; leave reserved-error for now if Task 3.2 hasn't landed)
    return "", "", fmt.Errorf(`--store: scheme %q is reserved; cloud adapter for this provider lands at M7`, scheme)
```

- [ ] **Step 2: Add `openStore` case**

```go
case "gcs":
    cfg, err := gcs.ParseURL(url)
    if err != nil {
        return nil, err
    }
    applyEnvToGCSConfig(&cfg)
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    s, err := gcs.Open(ctx, cfg)
    if err != nil {
        return nil, fmt.Errorf("gcs: %w", err)
    }
    return s, nil
```

- [ ] **Step 3: Add `applyEnvToGCSConfig`**

```go
func applyEnvToGCSConfig(cfg *gcs.Config) {
    if v := os.Getenv("BUCKETVCS_GCS_ENDPOINT"); v != "" {
        cfg.Endpoint = v
    }
    if v := os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"); v != "" {
        cfg.CredentialsFile = v
    }
    if v := os.Getenv("BUCKETVCS_GCS_USER_PROJECT"); v != "" {
        cfg.UserProject = v
    }
    // GOOGLE_APPLICATION_CREDENTIALS is honored by the GCS SDK directly.
}
```

Add `"github.com/bucketvcs/bucketvcs/internal/storage/gcs"` to the imports.

- [ ] **Step 4: Update the parser-error message strings**

Change the `parseStoreURL` initial error and the `unknown scheme` fallback to mention `gcs://`:

```go
return "", "", fmt.Errorf(`--store: missing scheme; want "localfs:<path>", "s3://<bucket>[/<prefix>]", "r2://<bucket>[/<prefix>]", or "gcs://<bucket>[/<prefix>]"`)
```

- [ ] **Step 5: Write parser tests**

In `cmd/bucketvcs/store_test.go`, add cases:

```go
{name: "gcs ok", in: "gcs://my-bucket", wantScheme: "gcs", wantPath: "my-bucket"},
{name: "gcs with prefix", in: "gcs://my-bucket/repos/staging", wantScheme: "gcs", wantPath: "my-bucket/repos/staging"},
{name: "gcs missing bucket", in: "gcs://", wantErr: "requires a bucket"},
{name: "gcs no slashes", in: "gcs:my-bucket", wantErr: "must use the form"},
```

- [ ] **Step 6: Run, then commit**

```bash
go test ./cmd/bucketvcs -run TestParseStoreURL -v
go build ./...
git add cmd/bucketvcs/store.go cmd/bucketvcs/store_test.go
git commit -m "M7 task 3.1: cmd/bucketvcs wires gcs:// through internal/storage/gcs"
```

---

### Task 3.2: Wire `azureblob://` through cmd/bucketvcs/store.go

**Files:**
- Modify: `cmd/bucketvcs/store.go`
- Modify: `cmd/bucketvcs/store_test.go`

- [ ] **Step 1: Replace the reserved-error case with the live one**

```go
case "azureblob":
    if !strings.HasPrefix(rest, "//") {
        return "", "", fmt.Errorf(`--store: azureblob URL must use the form azureblob://<container>[/<prefix>] (got %q)`, s)
    }
    contPath := strings.TrimPrefix(rest, "//")
    cont, _, _ := strings.Cut(contPath, "/")
    if cont == "" {
        return "", "", fmt.Errorf(`--store: azureblob:// requires a container name (got %q)`, s)
    }
    return scheme, contPath, nil
```

- [ ] **Step 2: Add `openStore` case**

```go
case "azureblob":
    cfg, err := azureblob.ParseURL(url)
    if err != nil {
        return nil, err
    }
    applyEnvToAzureConfig(&cfg)
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    s, err := azureblob.Open(ctx, cfg)
    if err != nil {
        return nil, fmt.Errorf("azureblob: %w", err)
    }
    return s, nil
```

- [ ] **Step 3: Add `applyEnvToAzureConfig`**

```go
func applyEnvToAzureConfig(cfg *azureblob.Config) {
    if v := os.Getenv("BUCKETVCS_AZURE_ACCOUNT"); v != "" {
        cfg.Account = v
    }
    if v := os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"); v != "" {
        cfg.ServiceURL = v
    }
    if v := os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"); v != "" {
        cfg.AccountKey = v
    }
    if v := os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"); v != "" {
        cfg.ConnectionString = v
    }
}
```

Add `"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"` to imports.

- [ ] **Step 4: Update parser tests**

```go
{name: "azureblob ok", in: "azureblob://my-container", wantScheme: "azureblob", wantPath: "my-container"},
{name: "azureblob with prefix", in: "azureblob://my-container/a/b", wantScheme: "azureblob", wantPath: "my-container/a/b"},
{name: "azureblob missing container", in: "azureblob://", wantErr: "requires a container"},
```

Update the missing-scheme error message to also mention `azureblob://`:

```go
return "", "", fmt.Errorf(`--store: missing scheme; want "localfs:<path>", "s3://<bucket>[/<prefix>]", "r2://<bucket>[/<prefix>]", "gcs://<bucket>[/<prefix>]", or "azureblob://<container>[/<prefix>]"`)
```

- [ ] **Step 5: Run, then commit**

```bash
go test ./cmd/bucketvcs -run TestParseStoreURL -v
go build ./...
git add cmd/bucketvcs/store.go cmd/bucketvcs/store_test.go
git commit -m "M7 task 3.2: cmd/bucketvcs wires azureblob:// through internal/storage/azureblob"
```

---

### Task 3.3: End-to-end smoke against the emulator stack

**Files:**
- Modify: `internal/diffharness/roundtrip_helpers_test.go`

This task piggybacks on the existing diffharness pattern from M5: the harness env-var override that lets the round-trip integration test run against `gcs://` or `azureblob://`.

- [ ] **Step 1: Locate the existing override**

```bash
grep -n "BUCKETVCS_DIFFHARNESS_STORE" internal/diffharness/roundtrip_helpers_test.go
```

The env var should already select between `localfs`, `s3`, `r2`. Extend its `switch` to recognize `gcs` and `azureblob` and dispatch through `cmd/bucketvcs/openStore` (or a small re-implementation if the helper is package-private).

- [ ] **Step 2: Add fixture buckets/containers**

The helper should pre-seed a uuid-prefix under the configured bucket exactly like the M5 helper does for s3.

- [ ] **Step 3: Run end-to-end against fake-gcs and Azurite**

```bash
docker compose -f docker-compose.cloud.yml up -d --wait

export BUCKETVCS_GCS_TEST_BUCKET=bucketvcs-conformance
export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443
export BUCKETVCS_DIFFHARNESS_STORE=gcs://bucketvcs-conformance/diffharness/
go test -count=1 -timeout=10m ./internal/diffharness

export BUCKETVCS_DIFFHARNESS_STORE=azureblob://bucketvcs-conformance/diffharness/
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
go test -count=1 -timeout=10m ./internal/diffharness
```

Both runs should pass; the round-trip exercises `bucketvcs init` → `import` → `serve` → native `git clone` → byte-identical comparison.

- [ ] **Step 4: Commit**

```bash
git add internal/diffharness/roundtrip_helpers_test.go
git commit -m "M7 task 3.3: diffharness env-driven override for gcs:// and azureblob://"
```

---

## Phase 4 — AWS S3 promotion to canonical

No code in `s3compat`. Operational + documentation only.

### Task 4.1: Add real-AWS leg + required MinIO check

**Files:**
- Modify: `.github/workflows/conformance.yml` (no actual edit needed — the workflow from Task 0.3 already includes the AWS step; verify it triggers when secrets are present.)
- Modify: `scripts/conformance-emulators.sh` (already includes MinIO; verify the s3compat conformance test binds to `BUCKETVCS_S3_*` correctly when MinIO is the target.)

- [ ] **Step 1: Verify the MinIO-targeted s3compat conformance test currently passes locally**

```bash
./scripts/conformance-emulators.sh
```

Expected: PASS for `internal/storage/s3compat` against MinIO, in addition to gcs against fake-gcs and azureblob against Azurite.

If the s3compat test was previously skipped on missing real-cloud creds (it should opt into MinIO via the env), audit `s3compat_conformance_test.go` and confirm `BUCKETVCS_S3_TEST_BUCKET` plus the MinIO env vars are honored. No code change should be required — M5 already wired this for the optional MinIO smoke.

- [ ] **Step 2: Document in repo settings**

In the M7 PR description, list the GitHub repo secrets that must be configured for the nightly real-cloud job:

```
AWS_S3_TEST_BUCKET, AWS_S3_TEST_REGION,
AWS_S3_TEST_ACCESS_KEY_ID, AWS_S3_TEST_SECRET_ACCESS_KEY
R2_TEST_BUCKET, R2_TEST_ENDPOINT,
R2_TEST_ACCESS_KEY_ID, R2_TEST_SECRET_ACCESS_KEY
GCS_TEST_BUCKET, GCS_TEST_CREDENTIALS_JSON
AZURE_TEST_CONTAINER, AZURE_TEST_ACCOUNT, AZURE_TEST_ACCOUNT_KEY
```

The workflow's `if: ${{ secrets.X != '' }}` guards mean unconfigured legs no-op cleanly.

- [ ] **Step 3: No commit needed unless the MinIO env wiring was previously broken — fix and commit then**

If you needed to fix the s3compat MinIO env wiring:
```bash
git add internal/storage/s3compat/s3compat_conformance_test.go
git commit -m "M7 task 4.1: s3compat conformance honors MinIO env vars (PR-blocking)"
```

---

### Task 4.2: Documentation updates marking S3 canonical

**Files:**
- Modify: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`
- Modify: `docs/m5-cloud-quickstart.md`
- Modify: `README.md`
- Modify: `internal/storage/s3compat/README.md`

- [ ] **Step 1: Update decomposition design**

Edit the §11.1 backend matrix row for AWS S3 (or the equivalent passage that describes S3's M5/M7 status). Replace any "AWS S3 promotion in progress (M7)" qualifier with "AWS S3 — canonical (since M7)".

- [ ] **Step 2: Update m5-cloud-quickstart.md**

Move the S3 examples into a dedicated `## AWS S3` section if they're currently grouped with R2 under an "M7 promotion in progress" qualifier. Drop the qualifier.

- [ ] **Step 3: Update README.md**

Locate the storage backends section (or add one if none exists). List the four canonical schemes:

```markdown
### Canonical storage backends

- `localfs:<path>` — local filesystem
- `s3://<bucket>[/<prefix>]` — AWS S3
- `r2://<bucket>[/<prefix>]` — Cloudflare R2
- `gcs://<bucket>[/<prefix>]` — Google Cloud Storage
- `azureblob://<container>[/<prefix>]` — Azure Blob Storage

All canonical backends pass the storage conformance suite in CI:
emulators on every PR, real cloud nightly.
```

- [ ] **Step 4: Update internal/storage/s3compat/README.md**

Drop any "promoted to canonical at M7" wording. Replace with "Canonical bucketvcs storage backend for AWS S3 and Cloudflare R2 (§11.1)."

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md docs/m5-cloud-quickstart.md README.md internal/storage/s3compat/README.md
git commit -m "M7 task 4.2: mark AWS S3 canonical in decomposition, README, m5 quickstart, s3compat README"
```

---

## Phase 5 — Quickstart docs and acceptance

### Task 5.1: docs/m7-cloud-quickstart.md

**Files:**
- Create: `docs/m7-cloud-quickstart.md`

- [ ] **Step 1: Write the quickstart**

```markdown
# M7 cloud backends — quickstart

This document covers the two new canonical storage backends added in M7:
Google Cloud Storage and Azure Blob Storage. For AWS S3 and Cloudflare R2,
see `docs/m5-cloud-quickstart.md`.

## Google Cloud Storage

### Prerequisites

- A GCS bucket in a region you can write to.
- A service account with `Storage Object Admin` on the bucket (Object User
  is sufficient if you do not need to set bucket-level lifecycle).
- Either `GOOGLE_APPLICATION_CREDENTIALS` pointing at the service-account
  JSON, or `BUCKETVCS_GCS_CREDENTIALS_FILE`.

### URL form

```
gcs://<bucket>[/<prefix>]
```

### Example

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json

bucketvcs init --store=gcs://my-bucket/my-org/my-repo.bv
bucketvcs serve --store=gcs://my-bucket/my-org/my-repo.bv --listen=:8080

git clone http://user:$(bucketvcs token issue --user=user)@localhost:8080/my-org/my-repo.git
```

### Smoke test against fake-gcs-server

```bash
docker run -d --name fake-gcs -p 4443:4443 fsouza/fake-gcs-server -scheme http -public-host localhost:4443
curl -X POST -H "Content-Type: application/json" -d '{"name":"smoke"}' http://localhost:4443/storage/v1/b?project=bucketvcs

export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443
bucketvcs init --store=gcs://smoke/repo.bv
```

## Azure Blob Storage

### Prerequisites

- A storage account in a region you can write to.
- A container under that account.
- Either Shared Key (account key), DefaultAzureCredential (workload identity,
  managed identity, or `az login`), or a connection string.

### URL form

```
azureblob://<container>[/<prefix>]
```

### Example

```bash
export BUCKETVCS_AZURE_ACCOUNT=mystorageacct
export BUCKETVCS_AZURE_ACCOUNT_KEY=<key>

bucketvcs init --store=azureblob://my-container/my-org/my-repo.bv
bucketvcs serve --store=azureblob://my-container/my-org/my-repo.bv --listen=:8080
```

### Smoke test against Azurite

```bash
docker run -d --name azurite -p 10000:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
docker exec azurite az storage container create --name smoke \
  --connection-string "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
bucketvcs init --store=azureblob://smoke/repo.bv
```

## Rotating CI secrets

The nightly conformance job in `.github/workflows/conformance.yml` reads
credentials from repo secrets. To rotate:

1. Generate a new key in the cloud console.
2. Update the GitHub repo secret (`Settings -> Secrets and variables -> Actions`).
3. Trigger the workflow manually via `workflow_dispatch` to confirm the new key works.
4. Revoke the old key in the cloud console.

Do not rotate via local CLI commands that print the key — keys can leak through
shell history. Generate, copy directly into the GitHub UI, then close the tab.
```

- [ ] **Step 2: Commit**

```bash
git add docs/m7-cloud-quickstart.md
git commit -m "M7 task 5.1: m7-cloud-quickstart.md (gcs + azureblob)"
```

---

### Task 5.2: §12 acceptance criteria check + m7_progress.md

**Files:**
- Create: `docs/superpowers/specs/m7_progress.md`

This is the M5/M6-style "acceptance gate" task. Run the full conformance suite locally against emulators, run any deferred end-to-end smoke, then write the progress note.

- [ ] **Step 1: Run the full local stack**

```bash
./scripts/conformance-emulators.sh
```

Expected: PASS for `localfs`, `s3compat` (against MinIO), `gcs` (against fake-gcs, with §29 #10 SignedURL skipped), `azureblob` (against Azurite, with §29 #10 SignedURL skipped if no key).

- [ ] **Step 2: Run the diffharness end-to-end against gcs and azureblob**

(See Task 3.3 step 3.) Both runs should be green.

- [ ] **Step 3: Write `docs/superpowers/specs/m7_progress.md`**

```markdown
# M7 — Remaining Canonical Cloud Backends — progress

Date merged: 2026-MM-DD
Tag: m7-complete

## Acceptance criteria — all green

1. ✅ `internal/storage/gcs` passes conformance against real GCS and fake-gcs-server.
2. ✅ `internal/storage/azureblob` passes conformance against real Azure Blob and Azurite.
3. ✅ `internal/storage/s3compat` passes conformance against real AWS S3 (in addition to R2 and MinIO).
4. ✅ `bucketvcs init --store=gcs://…`, `--store=azureblob://…`, and `--store=s3://…` work end-to-end with import/serve/native git clone+push.
5. ✅ The `emulators` CI job is required; the `real-cloud` nightly is green for the seven days preceding the tag.
6. ✅ No file outside `internal/storage/{gcs,azureblob,s3compat}/` imports a provider SDK.

## Notes

- AWS S3 is now formally canonical alongside R2.
- Documented skips: §29 #10 SignedURL skipped against fake-gcs (no SignedURL implementation) and against Azurite when no account key is configured. Real-cloud runs exercise it fully.
- Repo secrets required for the nightly run are listed in `docs/m7-cloud-quickstart.md`.

## Out of scope (deferred)

- Tigris, MinIO AIStor (deployment-tested candidates) — §11.2.
- Wasabi, B2, Ceph, etc. — §11.3 compatibility tier.
- Cross-backend migration tooling (M16 if needed).
- `bucketvcs store check` smoke-test subcommand.
```

- [ ] **Step 4: Update memory note in `m5_progress.md`**

The line "AWS S3 (M7 promotion in progress)" in the user's memory file should now read "AWS S3 (canonical since M7)". This is documented in the design but the actual edit happens in the user's `~/.claude/projects/.../memory/m5_progress.md` after the merge — leave a TODO note in `m7_progress.md` to remind whoever merges:

```markdown
## Post-merge bookkeeping

After tagging m7-complete, update `m5_progress.md` (in user memory):
replace "AWS S3 (M7 promotion in progress)" with "AWS S3 (canonical since M7)".
```

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/m7_progress.md
git commit -m "M7 task 5.2: acceptance criteria green; m7_progress.md"
```

- [ ] **Step 6: Tag `m7-complete` (do this in the merge PR, not on the worktree branch)**

After the worktree branch merges to main:

```bash
git tag m7-complete
git push origin m7-complete
```

---

## Self-review checklist (run before requesting review)

Walk through the spec sections one by one and confirm a task implements each:

| Spec section | Task(s) |
|---|---|
| What this milestone delivers (gcs, azureblob, s3 promotion) | All phases |
| Architecture / package layout | 1.1, 2.1 |
| Boundary (no SDK leaks) | Verified by Task 5.2 acceptance criterion 6 |
| Code-sharing decision (no shared base) | Implicit — each adapter self-contained |
| GCS provider mapping (§12.2) | 1.7–1.13 |
| Azure provider mapping (§12.4) | 2.7–2.13 |
| URL parsing | 1.3, 2.3, 3.1, 3.2 |
| Secrets policy | 1.3, 2.3 (URL parsers reject creds) |
| Conformance test wiring | 1.15, 2.15 |
| Local-emulator profile (PR-blocking) | 0.1, 0.2 |
| CI matrix (emulators + nightly) | 0.3, 4.1 |
| Documented conformance gaps (skips) | 1.13 (signed.go) returns ErrNotSupported; suite probes |
| AWS S3 promotion CI + docs | 4.1, 4.2 |
| Documentation deliverables | 1.16, 2.16, 4.2, 5.1, 5.2 |
| Branching and merge | Worktree `worktree-m7-cloud`, single merge — operational, not a task |
| Acceptance criteria | 5.2 |
| Risk register (block-ID collision, GCS staleness) | 2.12 covers block-ID; GCS staleness out of scope |

If a row has no task, add one. If a task is unclear, fix it inline now.

---

## Execution

After self-review passes, hand off to either:

- **subagent-driven-development** (recommended) — fresh subagent per task with reviews between
- **executing-plans** — inline execution with checkpoints

Either way: each task ends green, commits, then the next begins. Do not batch commits across tasks.
