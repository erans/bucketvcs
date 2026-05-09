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

(Tasks 1.5–1.14 — error classification, retry, Open, get/put/delete/list, multipart, signed URL, conformance, README — added next.)
