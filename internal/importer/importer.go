// Package importer round-trips a bare git repo from local disk into
// bucketvcs storage. See spec §3.6 for the import flow. The exported
// surface is Import(ctx, store, opts); the unexported helpers in this
// file are split out for testability.
package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures one import.
type Options struct {
	SourceDir     string
	Tenant, Repo  string
	Actor         string
	DefaultBranch string // optional; if empty, taken from source HEAD
}

// Result describes a successful import's resulting state.
type Result struct {
	PackID          string
	ObjectMapHash   string
	CommitGraphHash string
	ManifestVersion uint64
	RefCount        int
	ObjectCount     int
}

// preparedPack is the local-disk artifact set produced before any upload.
// Caller owns WorkDir and must clean it up.
type preparedPack struct {
	WorkDir       string
	PackID        string
	PackPath      string
	IdxPath       string
	Refs          map[string]string
	DefaultBranch string
}

// prepareLocalPack runs steps 1-3 + 5 of §3.6: clone, fsck, pack-objects,
// collect refs + default branch.
//
// If wantDefaultBranch is non-empty, the caller is overriding the source
// repo's HEAD; SymbolicRef failures (detached HEAD, etc.) are tolerated.
func prepareLocalPack(ctx context.Context, sourceDir, wantDefaultBranch string) (_ *preparedPack, retErr error) {
	work, err := os.MkdirTemp("", "bucketvcs-import-")
	if err != nil {
		return nil, fmt.Errorf("importer: tmpdir: %w", err)
	}
	defer func() {
		// On error, drop the workdir; on success, the caller takes ownership.
		if retErr != nil {
			_ = os.RemoveAll(work)
		}
	}()

	bare := filepath.Join(work, "src.git")
	if err := gitcli.CloneBareMirror(ctx, sourceDir, bare); err != nil {
		return nil, fmt.Errorf("importer: clone: %w", err)
	}
	if err := gitcli.Fsck(ctx, bare, true); err != nil {
		return nil, fmt.Errorf("importer: source fsck: %w", err)
	}

	// Resolve HEAD. If the source has detached HEAD, we need to write a
	// real on-disk ref BEFORE ShowRef and PackObjectsAll, so that
	// rev-list --all --objects in PackObjectsAll can discover the commit.
	headTarget, symErr := gitcli.SymbolicRef(ctx, bare, "HEAD")
	if symErr != nil {
		if wantDefaultBranch == "" {
			return nil, fmt.Errorf("importer: symbolic-ref HEAD: %w", symErr)
		}
		// Detached HEAD with caller-provided DefaultBranch: try to
		// resolve HEAD as a raw OID and write a real on-disk ref so
		// PackObjectsAll's rev-list sees the commit.
		headOID, rpErr := gitcli.RevParse(ctx, bare, "HEAD")
		if rpErr == nil {
			existingRefs, sErr := gitcli.ShowRef(ctx, bare)
			if sErr != nil {
				return nil, fmt.Errorf("importer: pre-synth show-ref: %w", sErr)
			}
			if _, exists := existingRefs[wantDefaultBranch]; exists {
				// Caller's DefaultBranch already exists in the source.
				// Leave it as the source author intended; we will use it
				// for HEAD downstream rather than synthesizing.
			} else {
				if err := gitcli.UpdateRef(ctx, bare, wantDefaultBranch, headOID); err != nil {
					return nil, fmt.Errorf("importer: synthesize ref %s -> %s: %w", wantDefaultBranch, headOID, err)
				}
			}
		}
		// If rpErr != nil, HEAD doesn't resolve (truly-empty repo).
		// Continue; ShowRef will return empty and the empty-repo path triggers.
		headTarget = ""
	}

	refs, err := gitcli.ShowRef(ctx, bare)
	if err != nil {
		return nil, fmt.Errorf("importer: show-ref: %w", err)
	}

	// Empty repo: no refs, no objects to pack. Skip pack-objects so
	// import can produce a manifest with empty packs/refs/indexes.
	if len(refs) == 0 {
		return &preparedPack{
			WorkDir:       work,
			PackID:        "",
			PackPath:      "",
			IdxPath:       "",
			Refs:          refs,
			DefaultBranch: headTarget,
		}, nil
	}
	prefix := filepath.Join(work, "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		return nil, fmt.Errorf("importer: mkdir pack out: %w", err)
	}
	packID, err := gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("importer: pack-objects: %w", err)
	}
	return &preparedPack{
		WorkDir:       work,
		PackID:        packID,
		PackPath:      prefix + "-" + packID + ".pack",
		IdxPath:       prefix + "-" + packID + ".idx",
		Refs:          refs,
		DefaultBranch: headTarget,
	}, nil
}

// localIndexes carries the bytes + content-hashes of .bvom and .bvcg
// produced from the local prepared pack. The bytes are uploaded as-is
// in step 6 of §3.6.
type localIndexes struct {
	ObjectMapBytes   []byte
	ObjectMapHash    string
	CommitGraphBytes []byte
	CommitGraphHash  string
	ObjectCount      int
	PackSizeBytes    int64
}

// buildIndexesLocal opens the local pack via pack.Reader (backed by a
// file-backed store), then calls objindex.Build and commitgraph.Build,
// hashing each output with SHA-256.
func buildIndexesLocal(ctx context.Context, prep *preparedPack) (*localIndexes, error) {
	if prep.PackID == "" {
		// Empty repo: no pack, no indexes.
		return &localIndexes{
			ObjectMapBytes:   nil,
			ObjectMapHash:    "",
			CommitGraphBytes: nil,
			CommitGraphHash:  "",
			ObjectCount:      0,
			PackSizeBytes:    0,
		}, nil
	}
	return buildIndexesFromPack(ctx, prep.PackPath, prep.IdxPath, prep.PackID, prep.Refs)
}

// buildIndexesFromPack is the granular core of buildIndexesLocal. It
// takes a single canonical pack on local disk + the manifest-truth ref
// map and produces the .bvom / .bvcg bytes plus their SHA-256 hashes.
//
// Both Import (via buildIndexesLocal) and BuildAndCommit call this. The
// pack is assumed to contain every object reachable from refs — caller
// is responsible for ensuring that (Import uses pack-objects --all on a
// fresh clone; BuildAndCommit uses repackToCanonical on the bare mirror).
func buildIndexesFromPack(ctx context.Context, packPath, idxPath, packID string, refs map[string]string) (*localIndexes, error) {
	store, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		return nil, fmt.Errorf("importer: localfile pack store: %w", err)
	}
	r, err := pack.Open(ctx, store, "p.pack", "p.idx")
	if err != nil {
		return nil, fmt.Errorf("importer: pack.Open: %w", err)
	}
	defer r.Close()

	// .bvom from pack idx.
	bvom, err := objindex.Build(r, packID)
	if err != nil {
		return nil, fmt.Errorf("importer: objindex.Build: %w", err)
	}
	bvomSum := sha256.Sum256(bvom)

	// .bvcg from pack: derive ref tips that point at commits.
	tips, err := buildTipsFromRefs(ctx, r, refs)
	if err != nil {
		return nil, fmt.Errorf("importer: buildTipsFromRefs: %w", err)
	}
	bvcg, err := commitgraph.Build(ctx, r, tips)
	if err != nil {
		return nil, fmt.Errorf("importer: commitgraph.Build: %w", err)
	}
	bvcgSum := sha256.Sum256(bvcg)

	st, err := os.Stat(packPath)
	if err != nil {
		return nil, fmt.Errorf("importer: stat pack: %w", err)
	}

	return &localIndexes{
		ObjectMapBytes:   bvom,
		ObjectMapHash:    hex.EncodeToString(bvomSum[:]),
		CommitGraphBytes: bvcg,
		CommitGraphHash:  hex.EncodeToString(bvcgSum[:]),
		ObjectCount:      r.Idx().Count(),
		PackSizeBytes:    st.Size(),
	}, nil
}

// buildTipsFromRefs filters refs down to those whose target is a commit
// in the pack. Annotated tags are dereferenced via the tag's `object` line.
func buildTipsFromRefs(ctx context.Context, r *pack.Reader, refs map[string]string) ([]commitgraph.Tip, error) {
	tips := make([]commitgraph.Tip, 0, len(refs))
	for ref, oidStr := range refs {
		oid, err := pack.ParseOID(oidStr)
		if err != nil {
			return nil, fmt.Errorf("ref %s: parse oid: %w", ref, err)
		}
		obj, err := r.Get(ctx, oid)
		if err != nil {
			return nil, fmt.Errorf("ref %s: get %s: %w", ref, oid, err)
		}
		// Bound tag dereferencing to avoid pathological self-referencing tags.
		const maxTagDepth = 16
		depth := 0
		for obj.Type == pack.TypeTag {
			if depth >= maxTagDepth {
				return nil, fmt.Errorf("ref %s: tag chain exceeds depth %d", ref, maxTagDepth)
			}
			depth++
			target, err := tagTarget(obj.Data)
			if err != nil {
				return nil, fmt.Errorf("ref %s: tag target: %w", ref, err)
			}
			oid = target
			obj, err = r.Get(ctx, oid)
			if err != nil {
				return nil, fmt.Errorf("ref %s: dereference tag: %w", ref, err)
			}
		}
		if obj.Type != pack.TypeCommit {
			// Skip non-commit refs (e.g. refs/notes/* containing trees).
			continue
		}
		tips = append(tips, commitgraph.Tip{Ref: ref, OID: oid})
	}
	return tips, nil
}

// tagTarget extracts the OID from a tag object's `object <hex>` line.
func tagTarget(body []byte) (pack.OID, error) {
	for len(body) > 0 {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			return pack.OID{}, fmt.Errorf("tag body missing newline")
		}
		line := body[:nl]
		body = body[nl+1:]
		if len(line) > 7 && string(line[:7]) == "object " {
			return pack.ParseOID(string(line[7:]))
		}
	}
	return pack.OID{}, fmt.Errorf("tag body missing 'object <oid>' line")
}

// Import is the public entry point. See spec §3.6.
//
// Atomicity & failure semantics:
//
// Successful Import advances the manifest from version 1 (Create) to
// version 2 (this Import's Commit) via M1's CAS. The order is:
//  1. prepareLocalPack + buildIndexesLocal (local)
//  2. repo.Create — claim the repo (atomic)
//  3. Upload pack, idx, .bvom, .bvcg via PutIfAbsent
//  4. repo.Commit — atomic CAS swap to the imported body
//
// Failure modes:
//   - Step 1-2 failure: nothing visible in storage (no claim made).
//   - Step 3 (upload) failure: repo is claimed at version 1 with empty
//     body, plus any uploads that succeeded. Retrying Import with the
//     same (tenant, repo) will hit ErrRepoExists. The orphaned repo
//     and partial uploads must be cleaned up before retry. M8 GC will
//     sweep tx records and uploads; an explicit `bucketvcs delete`
//     command (M-later) will offer manual cleanup.
//   - Step 4 (Commit) failure: same as step 3 — repo claimed, body
//     remains empty.
//
// This is a known M2 limitation. A future iteration may add a single-
// CAS create-with-body primitive in internal/repo to make Import
// fully atomic.
//
// Repos that already exist (any version) return ErrRepoExists.
func Import(ctx context.Context, store storage.ObjectStore, opts Options) (*Result, error) {
	if opts.SourceDir == "" || opts.Tenant == "" || opts.Repo == "" {
		return nil, fmt.Errorf("importer: SourceDir, Tenant, Repo required")
	}
	// Cheap pre-check: if the repo already exists, fail early before
	// paying the local clone+pack cost. The later repo.Create still
	// handles the race where the repo is created between this check
	// and our claim.
	if _, err := repo.Open(ctx, store, opts.Tenant, opts.Repo); err == nil {
		return nil, repoerrs.ErrRepoExists
	} else if !errors.Is(err, repoerrs.ErrRepoNotFound) {
		// Real storage error; surface it.
		return nil, fmt.Errorf("importer: pre-check open: %w", err)
	}
	prep, err := prepareLocalPack(ctx, opts.SourceDir, opts.DefaultBranch)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(prep.WorkDir)

	idx, err := buildIndexesLocal(ctx, prep)
	if err != nil {
		return nil, err
	}

	k, err := keys.NewRepo(opts.Tenant, opts.Repo)
	if err != nil {
		return nil, err
	}

	// Step 7a: claim the repo via Create BEFORE uploading anything.
	// This way ErrRepoExists fires before we waste pack uploads.
	defaultBranch := opts.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = prep.DefaultBranch
	}
	if defaultBranch == "" {
		defaultBranch = "refs/heads/main"
	}

	// Validate default_branch is a syntactically reasonable full ref name.
	if !validFullRefName(defaultBranch) {
		return nil, fmt.Errorf("importer: default_branch %q is not a valid full ref name (e.g., refs/heads/main)",
			defaultBranch)
	}
	if err := gitcli.CheckRefFormat(ctx, defaultBranch); err != nil {
		return nil, fmt.Errorf("importer: default_branch %q rejected by git check-ref-format: %w",
			defaultBranch, err)
	}
	// For non-empty repos, default_branch must also be present in source refs.
	if len(prep.Refs) > 0 {
		if _, ok := prep.Refs[defaultBranch]; !ok {
			return nil, fmt.Errorf("importer: default_branch %q not present in source refs (have %d refs)",
				defaultBranch, len(prep.Refs))
		}
	}

	r, err := repo.Create(ctx, store, opts.Tenant, opts.Repo, repo.CreateOptions{
		DefaultBranch: defaultBranch,
		ObjectFormat:  "sha1",
		Actor:         opts.Actor,
	})
	if err != nil {
		return nil, err
	}

	// Step 6: now that we own the repo, upload (PutIfAbsent) in order:
	// pack, idx, .bvom, .bvcg. Skip uploads if the repo is empty.
	var packs []manifest.PackEntry
	indexes := manifest.Indexes{}
	if prep.PackID != "" {
		if err := uploadFile(ctx, store, prep.PackPath, k.CanonicalPackKey(prep.PackID)); err != nil {
			return nil, fmt.Errorf("importer: upload pack: %w", err)
		}
		if err := uploadFile(ctx, store, prep.IdxPath, k.PackIdxKey(prep.PackID, "canonical")); err != nil {
			return nil, fmt.Errorf("importer: upload idx: %w", err)
		}
		bvomKey := k.ObjectMapKey(idx.ObjectMapHash)
		if err := uploadBytes(ctx, store, idx.ObjectMapBytes, bvomKey); err != nil {
			return nil, fmt.Errorf("importer: upload .bvom: %w", err)
		}
		bvcgKey := k.CommitGraphKey(idx.CommitGraphHash)
		if err := uploadBytes(ctx, store, idx.CommitGraphBytes, bvcgKey); err != nil {
			return nil, fmt.Errorf("importer: upload .bvcg: %w", err)
		}
		packs = []manifest.PackEntry{{
			PackID:      prep.PackID,
			PackKey:     k.CanonicalPackKey(prep.PackID),
			IdxKey:      k.PackIdxKey(prep.PackID, "canonical"),
			SizeBytes:   idx.PackSizeBytes,
			ObjectCount: idx.ObjectCount,
		}}
		indexes = manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: bvomKey, Hash: idx.ObjectMapHash},
			CommitGraph: &manifest.IndexRef{Key: bvcgKey, Hash: idx.CommitGraphHash},
		}
	}

	// Step 7b: Commit the body.
	body := manifest.Body{
		DefaultBranch: defaultBranch,
		Refs:          prep.Refs,
		Packs:         packs,
		Indexes:       indexes,
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		return nil, err
	}
	commitTxBody := tx.Body{Type: "import", Actor: opts.Actor}
	if _, err := r.Commit(ctx, commitTxBody, func(prev *repo.RootView) ([]byte, error) {
		// Validate prev is still the empty Create-only state on every CAS
		// attempt. A concurrent committer between our Create and Commit
		// would otherwise be silently overwritten.
		if prev.Header.ManifestVersion != 1 {
			return nil, fmt.Errorf("importer: concurrent commit detected (manifest_version=%d, want 1)",
				prev.Header.ManifestVersion)
		}
		var existingBody manifest.Body
		if jerr := json.Unmarshal(prev.Body, &existingBody); jerr != nil {
			return nil, fmt.Errorf("importer: unmarshal prev body: %w", jerr)
		}
		// Include Bundles in the populated-body check so a version-1 body
		// with bundles is correctly rejected.
		if len(existingBody.Refs) > 0 || len(existingBody.Packs) > 0 ||
			len(existingBody.Bundles) > 0 ||
			existingBody.Indexes.ObjectMap != nil || existingBody.Indexes.CommitGraph != nil {
			return nil, fmt.Errorf("importer: concurrent writer populated body before import Commit")
		}
		return bodyBytes, nil
	}); err != nil {
		return nil, fmt.Errorf("importer: Commit: %w", err)
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("importer: ReadRoot post-commit: %w", err)
	}

	return &Result{
		PackID:          prep.PackID,
		ObjectMapHash:   idx.ObjectMapHash,
		CommitGraphHash: idx.CommitGraphHash,
		ManifestVersion: view.Header.ManifestVersion,
		RefCount:        len(prep.Refs),
		ObjectCount:     idx.ObjectCount,
	}, nil
}

// uploadFile streams a local file to the given key via PutIfAbsent.
//
// Pack/idx files are NOT content-addressed by their bytes (pack_id is
// based on the object set, not the bytes), so byte-level ErrAlreadyExists
// is NOT safe to treat as success — different deltifications can produce
// the same pack_id with different bytes. Pack-level conflicts surface to
// the caller; operators must clean up partial state before retrying.
func uploadFile(ctx context.Context, store storage.ObjectStore, srcPath, dstKey string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := store.PutIfAbsent(ctx, dstKey, f, nil); err != nil {
		return err
	}
	return nil
}

// uploadBytes uploads in-memory bytes via PutIfAbsent. The bytes ARE
// content-addressed (SHA-256 hash is part of the key), so an existing
// object with the same key necessarily has the same bytes — ErrAlreadyExists
// is safe to treat as success here.
func uploadBytes(ctx context.Context, store storage.ObjectStore, b []byte, dstKey string) error {
	if _, err := store.PutIfAbsent(ctx, dstKey, bytes.NewReader(b), nil); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

// validFullRefName performs a lightweight syntactic check on a full git
// ref name. It rejects empty strings, refs not starting with "refs/",
// path-traversal segments, and characters git's check-ref-format
// considers invalid. It's not a full reimplementation of Git's rules —
// callers can rely on git itself for downstream operations — but
// catches obviously-wrong inputs at the bucketvcs API boundary.
func validFullRefName(s string) bool {
	if s == "" || !strings.HasPrefix(s, "refs/") {
		return false
	}
	if strings.Contains(s, "//") || strings.Contains(s, "/.") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".lock") {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '~' || c == '^' || c == ':' || c == '?' || c == '*' || c == '[' || c == 0x7f {
			return false
		}
	}
	return true
}
