# GoReleaser Tagged Release Process Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hand-rolled release workflow with a GoReleaser pipeline that, on a semver tag, cross-compiles Windows/macOS/Linux binaries, builds `.deb`/`.rpm` packages, and auto-flags `-rcN` tags as pre-releases — and bump the toolchain to Go 1.26.3.

**Architecture:** A new `.goreleaser.yaml` at the repo root drives all builds, archives, Linux packages (built-in nfpm — no Docker), checksums, changelog, and release publishing. `.github/workflows/release.yml` keeps its `test` gate job but replaces the bash build/publish steps with `goreleaser/goreleaser-action`. `release.prerelease: auto` handles the pre-release distinction. The Go bump touches `go.mod` and every `setup-go` step across the four workflows.

**Tech Stack:** Go 1.26.3, GoReleaser v2 (nfpm built in), GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-05-29-goreleaser-release-process-design.md`

---

## File Structure

- **Create** `.goreleaser.yaml` — single source of truth for builds, archives, nfpm packages, checksums, changelog, release flags.
- **Modify** `go.mod` — `go 1.25.0` → `go 1.26.0`.
- **Modify** `.github/workflows/release.yml` — bump Go; replace bash build/publish with goreleaser-action; add `fetch-depth: 0`.
- **Modify** `.github/workflows/ci.yml` — bump 5 `go-version` lines.
- **Modify** `.github/workflows/conformance.yml` — bump 4 `go-version` lines.
- **Modify** `.github/workflows/conformance-cloud.yml` — bump 1 `go-version` line.

Note: this is a CI/packaging change. The "tests" are `go vet`, `go test`, `goreleaser check`, and a `goreleaser release --snapshot` dry-run — these are the verification gates in place of unit tests.

---

## Task 1: Bump toolchain to Go 1.26.3

**Files:**
- Modify: `go.mod` (the `go` directive line)
- Modify: `.github/workflows/release.yml` (2 `go-version` lines — will be rewritten in Task 3, bump here too so the repo is consistent at every commit)
- Modify: `.github/workflows/ci.yml` (5 lines)
- Modify: `.github/workflows/conformance.yml` (4 lines)
- Modify: `.github/workflows/conformance-cloud.yml` (1 line)

- [ ] **Step 1: Bump the `go.mod` directive**

Change the directive in `go.mod`:

```
go 1.26.0
```

(was `go 1.25.0`)

- [ ] **Step 2: Bump every workflow `go-version`**

Both quote styles are in use (`"1.25"` and `'1.25'`), so run two replacements:

```bash
cd /home/eran/work/bucketvcs
sed -i 's/go-version: "1.25"/go-version: "1.26.3"/g' \
  .github/workflows/release.yml \
  .github/workflows/ci.yml \
  .github/workflows/conformance.yml
sed -i "s/go-version: '1.25'/go-version: '1.26.3'/g" \
  .github/workflows/conformance-cloud.yml
```

- [ ] **Step 3: Verify no `1.25` go-version lines remain**

Run: `grep -rn "go-version" .github/workflows/`
Expected: every line shows `1.26.3`; no `1.25` remains.

- [ ] **Step 4: Verify the build and tests pass on the new directive**

Run: `cd /home/eran/work/bucketvcs && go vet ./... && go test -count=1 ./...`
Expected: PASS. (If local Go is < 1.26, `GOTOOLCHAIN=auto` — the default — fetches go1.26.x automatically.)

- [ ] **Step 5: Commit**

```bash
cd /home/eran/work/bucketvcs
git add go.mod .github/workflows/release.yml .github/workflows/ci.yml \
  .github/workflows/conformance.yml .github/workflows/conformance-cloud.yml
git commit -m "build: bump toolchain to Go 1.26.3"
```

---

## Task 2: Add `.goreleaser.yaml`

**Files:**
- Create: `.goreleaser.yaml`

- [ ] **Step 1: Write the GoReleaser config**

Create `/home/eran/work/bucketvcs/.goreleaser.yaml` with exactly this content:

```yaml
# GoReleaser configuration — https://goreleaser.com
# Validate:  goreleaser check
# Dry-run :  goreleaser release --snapshot --clean   (builds everything, publishes nothing)
version: 2

project_name: bucketvcs

before:
  hooks:
    - go mod tidy

builds:
  - id: bucketvcs
    main: ./cmd/bucketvcs
    binary: bucketvcs
    env:
      - CGO_ENABLED=0
    flags:
      - -trimpath
    ldflags:
      - -s -w -X main.buildVersion={{ .Version }}
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    ignore:
      # No Windows/arm64 target (matches the current release matrix).
      - goos: windows
        goarch: arm64

archives:
  - id: default
    formats:
      - tar.gz
    format_overrides:
      - goos: windows
        formats:
          - zip
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    files:
      - README.md
      - LICENSE
      - NOTICE
      - THIRD_PARTY_LICENSES

nfpms:
  - id: packages
    package_name: bucketvcs
    formats:
      - deb
      - rpm
    maintainer: "Eran Sandler <eran@sandler.co.il>"
    description: "bucketvcs — object-storage-backed Git server."
    license: "Apache-2.0"
    homepage: "https://github.com/bucketvcs/bucketvcs"
    bindir: /usr/bin
    contents:
      - src: README.md
        dst: /usr/share/doc/bucketvcs/README.md
      - src: LICENSE
        dst: /usr/share/doc/bucketvcs/LICENSE
      - src: NOTICE
        dst: /usr/share/doc/bucketvcs/NOTICE
      - src: THIRD_PARTY_LICENSES
        dst: /usr/share/doc/bucketvcs/THIRD_PARTY_LICENSES

checksum:
  name_template: checksums.txt

changelog:
  use: github

release:
  # Tags with a semver prerelease suffix (-rc1, -beta, -alpha) → GitHub pre-release;
  # clean vX.Y.Z tags → full release.
  prerelease: auto
```

- [ ] **Step 2: Install GoReleaser v2 locally (for verification)**

Run: `go install github.com/goreleaser/goreleaser/v2@latest`
Then ensure it is on PATH: `export PATH="$(go env GOPATH)/bin:$PATH" && goreleaser --version`
Expected: prints a GoReleaser v2.x version.

- [ ] **Step 3: Validate the config**

Run: `cd /home/eran/work/bucketvcs && goreleaser check`
Expected: `1 configuration file(s) validated` / "config is valid". Fix any reported field errors before continuing.

- [ ] **Step 4: Dry-run a full build (no publish)**

Run: `cd /home/eran/work/bucketvcs && goreleaser release --snapshot --clean`
Expected: completes successfully and `dist/` contains:
- 5 archives: `bucketvcs_*_linux_amd64.tar.gz`, `*_linux_arm64.tar.gz`, `*_darwin_amd64.tar.gz`, `*_darwin_arm64.tar.gz`, `*_windows_amd64.zip`
- 4 packages: `bucketvcs_*_amd64.deb`, `*_arm64.deb`, `*_amd64.rpm`, `*_arm64.rpm`
- `checksums.txt`

Verify: `ls dist/` shows the archives, `.deb`, `.rpm`, and `checksums.txt` above.

- [ ] **Step 5: Confirm version stamping works**

Run:
```bash
cd /home/eran/work/bucketvcs
tar -xzf dist/bucketvcs_*_linux_amd64.tar.gz -O bucketvcs > /tmp/bvcs_check 2>/dev/null || \
  tar -xzf "$(ls dist/*linux_amd64.tar.gz)" -C /tmp bucketvcs
/tmp/bucketvcs serve --version 2>/dev/null || echo "no --version subcommand; buildVersion is stamped via ldflags and surfaced through the gateway agent= string"
```
Expected: the binary runs; `buildVersion` is set from the snapshot version (snapshot tags look like `0.0.0-next`). Real version stamping is exercised by the tag-driven run.

- [ ] **Step 6: Verify the dry-run output is git-ignored, then commit the config**

Run: `cd /home/eran/work/bucketvcs && grep -q '^dist/$\|^dist$\|^/dist' .gitignore || echo "dist/" >> .gitignore`
Then:
```bash
cd /home/eran/work/bucketvcs
rm -rf dist
git add .goreleaser.yaml .gitignore
git commit -m "ci: add GoReleaser config (binaries, deb/rpm, prerelease auto)"
```

---

## Task 3: Rewrite `release.yml` to use GoReleaser

**Files:**
- Modify: `.github/workflows/release.yml` (replace the `release` job's build/publish steps)

- [ ] **Step 1: Replace the workflow with the GoReleaser-driven version**

Overwrite `/home/eran/work/bucketvcs/.github/workflows/release.yml` with exactly this content:

```yaml
name: release

on:
  push:
    tags:
      - 'v*'   # semver release tags only (v0.1.0, v1.2.3, v1.2.3-rc1). Milestone tags (mNN-*) are ignored.

permissions:
  contents: read

jobs:
  test:
    name: test gate
    runs-on: ubuntu-latest
    timeout-minutes: 25
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26.3"
          cache: true
      # Test fixtures shell out to `git commit`; the runner needs an identity.
      - name: Configure git identity
        run: |
          git config --global user.email "ci@bucketvcs.invalid"
          git config --global user.name "bucketvcs CI"
      - run: go vet ./...
      - run: go test -count=1 ./...

  release:
    name: build + publish
    needs: test
    runs-on: ubuntu-latest
    timeout-minutes: 20
    permissions:
      contents: write   # required to create the GitHub Release
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # full history + tags for changelog / version derivation
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26.3"
          cache: true
      # GoReleaser cross-compiles all five targets (CGO_ENABLED=0), builds the
      # .deb/.rpm via built-in nfpm, writes checksums, and publishes the GitHub
      # Release. A -rcN tag is auto-flagged as a pre-release (release.prerelease: auto).
      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          distribution: goreleaser
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 2: Re-validate the config against the same GoReleaser line the workflow uses**

Run: `cd /home/eran/work/bucketvcs && goreleaser check`
Expected: config is valid. (The workflow pins `~> v2`; the locally-installed v2 from Task 2 matches.)

- [ ] **Step 3: Sanity-check the workflow YAML parses**

Run: `cd /home/eran/work/bucketvcs && python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/release.yml')); print('release.yml OK')"`
Expected: `release.yml OK`

- [ ] **Step 4: Commit**

```bash
cd /home/eran/work/bucketvcs
git add .github/workflows/release.yml
git commit -m "ci: drive releases with GoReleaser (deb/rpm + prerelease auto)"
```

---

## Task 4: End-to-end pre-release rehearsal (optional, recommended before first real tag)

**Files:** none (operational verification)

- [ ] **Step 1: Push a pre-release tag**

```bash
cd /home/eran/work/bucketvcs
git tag v0.0.1-rc1
git push origin v0.0.1-rc1
```

- [ ] **Step 2: Watch the workflow**

Run: `gh run watch "$(gh run list --workflow=release.yml --limit 1 --json databaseId -q '.[0].databaseId')"`
Expected: `test` gate passes, then `release` succeeds.

- [ ] **Step 3: Confirm it published as a PRE-RELEASE with all artifacts**

Run: `gh release view v0.0.1-rc1 --json isPrerelease,assets -q '{prerelease: .isPrerelease, assets: [.assets[].name]}'`
Expected: `prerelease: true`, and assets include 5 archives, `*_amd64.deb`, `*_arm64.deb`, `*_amd64.rpm`, `*_arm64.rpm`, `checksums.txt`.

- [ ] **Step 4: Tear down the rehearsal release + tag**

```bash
cd /home/eran/work/bucketvcs
gh release delete v0.0.1-rc1 --yes --cleanup-tag
```
Expected: release and remote tag removed. (A subsequent clean `vX.Y.Z` tag will publish a full, non-pre-release.)

---

## Self-Review Notes

- **Spec coverage:** §1 trigger/versioning → Task 2 (`prerelease: auto`) + Task 3 (`v*` trigger, ldflags `buildVersion`); §2 build matrix → Task 2 builds block; §3 artifacts (archives/deb/rpm/checksums/notes) → Task 2 archives+nfpms+checksum+changelog; §4 workflow → Task 3; §5 Go 1.26 → Task 1; §6 verification → Task 2 Steps 3-5 + Task 4. All acceptance criteria mapped.
- **Placeholder scan:** none — every config field, command, and expected output is concrete.
- **Consistency:** `main.buildVersion` ldflag matches the var at `cmd/bucketvcs/serve.go:41`; `1.26.3` used uniformly across `go.mod` (`1.26.0` directive) and all workflows; archive/package names follow GoReleaser defaults referenced in the verification steps.
