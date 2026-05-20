package receivepack

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
)

// buildDelta walks the new commits introduced by this push (identified
// by revListNewOIDs — the output of RevListCommitsOnly from the receive
// pipeline), computes generation numbers using gl as base, and returns
// a Delta suitable for encoding as a .bvrd file.
//
// bareDir must already contain the new objects (after IndexPackStrict).
// packIDs is the list of pack OIDs in the new canonical manifest.
// revListNewOIDs must contain only commit OIDs (no trees/blobs) — callers
// should use gitcli.RevListCommitsOnly to produce this list.
//
// Unlike the plan's idealized "pure function / no IO", this
// implementation shells out to git (CatFilePretty) to read commit
// bodies from the bare — the pack.Reader path would require opening the
// inbound raw pack (before repack) while we only have the indexed bare
// available at integration time. Generation computation itself is pure.
func buildDelta(
	ctx context.Context,
	bareDir string,
	revListNewOIDs []string,
	gl *reachability.GenLookup,
	updates []updateCommand,
	packIDs []pack.OID,
) (deltaindex.Delta, error) {
	type commitNode struct {
		oid     pack.OID
		parents []pack.OID
	}
	newCommits := make([]commitNode, 0, len(revListNewOIDs))

	// revListNewOIDs contains only commits (no trees/blobs) because the caller
	// uses RevListCommitsOnly (no --objects flag). We parse each commit body to
	// extract parents; no per-OID type probe is needed.
	for _, oidStr := range revListNewOIDs {
		oid, err := pack.ParseOID(oidStr)
		if err != nil {
			return deltaindex.Delta{}, fmt.Errorf("buildDelta: parse oid %q: %w", oidStr, err)
		}
		body, err := gitcli.CatFilePretty(ctx, bareDir, oidStr)
		if err != nil {
			return deltaindex.Delta{}, fmt.Errorf("buildDelta: cat-file %s: %w", oidStr, err)
		}
		parents, err := parseCommitParents(body)
		if err != nil {
			return deltaindex.Delta{}, fmt.Errorf("buildDelta: parse parents %s: %w", oidStr, err)
		}
		newCommits = append(newCommits, commitNode{oid: oid, parents: parents})
	}

	// Index new commits by OID for fast parent traversal.
	byOID := make(map[pack.OID]int, len(newCommits))
	for i := range newCommits {
		byOID[newCommits[i].oid] = i
	}

	// Topological generation computation: gen(c) = max(gen(parents)) + 1.
	// Lookup order: already-computed new commits, then gl (base + prior deltas),
	// then unknown (treated as gen 0, yielding gen 1 for root commits).
	gens := make(map[pack.OID]uint32, len(newCommits))
	visiting := make(map[pack.OID]bool, len(newCommits))
	var visit func(oid pack.OID) uint32
	visit = func(oid pack.OID) uint32 {
		if g, ok := gens[oid]; ok {
			return g
		}
		if visiting[oid] {
			// Cycle guard: commit OIDs are content-addressed so cycles should
			// never occur, but fail safe (treat as gen 0) if they do.
			return 0
		}
		if g, ok := gl.Lookup(oid); ok {
			gens[oid] = g
			return g
		}
		idx, inPack := byOID[oid]
		if !inPack {
			// Parent not in new commits and not in base — unknown, treat as 0.
			gens[oid] = 0
			return 0
		}
		visiting[oid] = true
		defer delete(visiting, oid)
		var maxParent uint32
		for _, p := range newCommits[idx].parents {
			if g := visit(p); g > maxParent {
				maxParent = g
			}
		}
		g := maxParent + 1
		gens[oid] = g
		return g
	}
	for _, c := range newCommits {
		visit(c.oid)
	}

	records := make([]deltaindex.CommitRecord, 0, len(newCommits))
	for _, c := range newCommits {
		records = append(records, deltaindex.CommitRecord{
			OID:        c.oid,
			Generation: gens[c.oid],
			Parents:    c.parents,
		})
	}

	// Build ref-tip diffs: include all accepted (non-ng) updates, including
	// deletions (NewOID == oidconst.NullOIDHex). Deletions are represented by a zero NewOID
	// so that Set.Load can remove the ref from the effective ref-tip map when
	// replaying the delta chain.
	tips := make([]deltaindex.RefTipDiff, 0, len(updates))
	for _, u := range updates {
		newOIDStr := u.NewOID
		var newOID pack.OID // zero value = deletion
		if newOIDStr != "" && newOIDStr != oidconst.NullOIDHex {
			var err error
			if newOID, err = pack.ParseOID(newOIDStr); err != nil {
				return deltaindex.Delta{}, fmt.Errorf("buildDelta: parse new OID %q: %w", newOIDStr, err)
			}
		}
		var oldOID pack.OID // zero value = creation
		if u.OldOID != "" && u.OldOID != oidconst.NullOIDHex {
			var err error
			if oldOID, err = pack.ParseOID(u.OldOID); err != nil {
				return deltaindex.Delta{}, fmt.Errorf("buildDelta: parse old OID %q: %w", u.OldOID, err)
			}
		}
		tips = append(tips, deltaindex.RefTipDiff{
			RefName: u.Refname,
			OldOID:  oldOID,
			NewOID:  newOID, // zero OID = deletion
		})
	}
	// Sort tips by RefName so that two pushes with identical content but
	// different client ref-update ordering produce the same .bvrd hash.
	sort.Slice(tips, func(i, j int) bool {
		return tips[i].RefName < tips[j].RefName
	})

	return deltaindex.Delta{
		Commits: records,
		RefTips: tips,
		Packs:   packIDs,
	}, nil
}

// parseCommitParents extracts parent OIDs from a git commit body (the
// output of `git cat-file -p <oid>`). Header lines: tree <hex>\n;
// parent <hex>\n (zero or more, immediately after tree); stops at the
// first non-tree-non-parent header line.
func parseCommitParents(body []byte) ([]pack.OID, error) {
	var parents []pack.OID
	for len(body) > 0 {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			return parents, nil
		}
		line := body[:nl]
		if bytes.HasPrefix(line, []byte("tree ")) {
			body = body[nl+1:]
			continue
		}
		if bytes.HasPrefix(line, []byte("parent ")) {
			hexStr := string(bytes.TrimPrefix(line, []byte("parent ")))
			oid, err := pack.ParseOID(hexStr)
			if err != nil {
				return nil, fmt.Errorf("parse parent %q: %w", hexStr, err)
			}
			parents = append(parents, oid)
			body = body[nl+1:]
			continue
		}
		// First non-tree-non-parent header line — stop.
		break
	}
	return parents, nil
}
