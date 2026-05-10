package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// LocalIndexes is the result of buildIndexesFromLocalPack — exactly the
// shape the maintenance pipeline hands to the upload + CAS-merge phases.
type LocalIndexes struct {
	ObjectMapBytes   []byte
	ObjectMapHash    string // hex-encoded SHA-256
	CommitGraphBytes []byte
	CommitGraphHash  string // hex-encoded SHA-256
	ObjectCount      int
	PackSizeBytes    int64
}

// buildIndexesFromLocalPack opens (packPath, idxPath) via the local
// file pack store, builds .bvom (objindex) and .bvcg (commit-graph),
// and returns the bytes + their content hashes.
//
// Mirrors internal/importer.buildIndexesFromPack. Caller is responsible
// for ensuring the pack is reachability-complete relative to refs
// (Phase 1+2 of the maintenance pipeline guarantee this).
func buildIndexesFromLocalPack(ctx context.Context, packPath, idxPath, packID string, refs map[string]string) (*LocalIndexes, error) {
	store, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		return nil, fmt.Errorf("indexes: localpack: %w", err)
	}
	r, err := pack.Open(ctx, store, "p.pack", "p.idx")
	if err != nil {
		return nil, fmt.Errorf("indexes: pack.Open: %w", err)
	}
	defer r.Close()

	bvom, err := objindex.Build(r, packID)
	if err != nil {
		return nil, fmt.Errorf("indexes: objindex.Build: %w", err)
	}
	bvomSum := sha256.Sum256(bvom)

	tips, err := buildTipsFromRefs(ctx, r, refs)
	if err != nil {
		return nil, fmt.Errorf("indexes: buildTipsFromRefs: %w", err)
	}
	bvcg, err := commitgraph.Build(ctx, r, tips)
	if err != nil {
		return nil, fmt.Errorf("indexes: commitgraph.Build: %w", err)
	}
	bvcgSum := sha256.Sum256(bvcg)

	st, err := os.Stat(packPath)
	if err != nil {
		return nil, fmt.Errorf("indexes: stat pack: %w", err)
	}
	return &LocalIndexes{
		ObjectMapBytes:   bvom,
		ObjectMapHash:    hex.EncodeToString(bvomSum[:]),
		CommitGraphBytes: bvcg,
		CommitGraphHash:  hex.EncodeToString(bvcgSum[:]),
		ObjectCount:      r.Idx().Count(),
		PackSizeBytes:    st.Size(),
	}, nil
}

// buildTipsFromRefs filters refs to those whose target is a commit in
// the pack (annotated tags are dereferenced via the tag's `object`
// line, capped at depth 16). Mirrors internal/importer's helper.
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
		const maxTagDepth = 16
		depth := 0
		for obj.Type == pack.TypeTag {
			depth++
			if depth > maxTagDepth {
				return nil, fmt.Errorf("ref %s: tag chain exceeds depth %d", ref, maxTagDepth)
			}
			target, err := tagTargetOID(obj.Data)
			if err != nil {
				return nil, fmt.Errorf("ref %s: tag parse: %w", ref, err)
			}
			oid = target
			obj, err = r.Get(ctx, oid)
			if err != nil {
				return nil, fmt.Errorf("ref %s: tag target %s: %w", ref, target, err)
			}
		}
		if obj.Type != pack.TypeCommit {
			continue // skip non-commit refs (e.g. blob ref) silently
		}
		tips = append(tips, commitgraph.Tip{Ref: ref, OID: oid})
	}
	return tips, nil
}

// tagTargetOID parses a tag body's `object <oid>` header. Mirrors
// internal/importer.tagTarget; copied to avoid an importer dependency.
func tagTargetOID(body []byte) (pack.OID, error) {
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
