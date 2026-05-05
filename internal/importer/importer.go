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
	"fmt"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
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
func prepareLocalPack(ctx context.Context, sourceDir string) (_ *preparedPack, retErr error) {
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
	prefix := filepath.Join(work, "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		return nil, fmt.Errorf("importer: mkdir pack out: %w", err)
	}
	packID, err := gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("importer: pack-objects: %w", err)
	}
	refs, err := gitcli.ShowRef(ctx, bare)
	if err != nil {
		return nil, fmt.Errorf("importer: show-ref: %w", err)
	}
	headTarget, err := gitcli.SymbolicRef(ctx, bare, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("importer: symbolic-ref HEAD: %w", err)
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
	store, err := newLocalFilePackStore(prep.PackPath, prep.IdxPath)
	if err != nil {
		return nil, fmt.Errorf("importer: localfile pack store: %w", err)
	}
	r, err := pack.Open(ctx, store, "p.pack", "p.idx")
	if err != nil {
		return nil, fmt.Errorf("importer: pack.Open: %w", err)
	}
	defer r.Close()

	// .bvom from pack idx.
	bvom, err := objindex.Build(r, prep.PackID)
	if err != nil {
		return nil, fmt.Errorf("importer: objindex.Build: %w", err)
	}
	bvomSum := sha256.Sum256(bvom)

	// .bvcg from pack: derive ref tips that point at commits.
	tips, err := buildTipsFromRefs(ctx, r, prep.Refs)
	if err != nil {
		return nil, fmt.Errorf("importer: buildTipsFromRefs: %w", err)
	}
	bvcg, err := commitgraph.Build(ctx, r, tips)
	if err != nil {
		return nil, fmt.Errorf("importer: commitgraph.Build: %w", err)
	}
	bvcgSum := sha256.Sum256(bvcg)

	st, err := os.Stat(prep.PackPath)
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
