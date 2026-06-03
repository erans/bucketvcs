package gitbrowse

import (
	"context"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

const (
	maxDiffFiles        = 300
	maxDiffLinesPerFile = 3000
)

// Commit returns the commit metadata, message, parents, and parsed diff.
func (s *Service) Commit(ctx context.Context, tenant, repoID, oid string) (browsemodel.CommitDetail, error) {
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.CommitDetail{}, err
	}
	defer release()

	rawCommit, err := gitcli.CatFileCommit(ctx, m.BareDir(), oid)
	if err != nil {
		return browsemodel.CommitDetail{}, browsemodel.ErrNotFound
	}
	meta, parents, msg, err := parseCommitObject(rawCommit)
	if err != nil {
		return browsemodel.CommitDetail{}, err
	}
	meta.OID = oid
	meta.ShortOID = oid
	if len(oid) > 12 {
		meta.ShortOID = oid[:12]
	}

	rawDiff, err := gitcli.DiffTreePatch(ctx, m.BareDir(), oid)
	if err != nil {
		return browsemodel.CommitDetail{}, err
	}
	files, truncated := parseUnifiedDiff(rawDiff)
	return browsemodel.CommitDetail{
		Meta: meta, Message: msg, Parents: parents, Files: files, Truncated: truncated,
	}, nil
}

// parseCommitObject parses a raw `git cat-file commit` object.
func parseCommitObject(raw []byte) (browsemodel.CommitMeta, []string, string, error) {
	s := string(raw)
	var parents []string
	var meta browsemodel.CommitMeta
	idx := strings.Index(s, "\n\n")
	header := s
	msg := ""
	if idx >= 0 {
		header = s[:idx]
		msg = s[idx+2:]
	}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "parent "):
			parents = append(parents, strings.TrimSpace(line[len("parent "):]))
		case strings.HasPrefix(line, "author "):
			name, email, when := parseIdentity(line[len("author "):])
			meta.AuthorName, meta.AuthorEmail, meta.AuthorTime = name, email, when
		}
	}
	meta.Summary = msg
	if nl := strings.IndexByte(msg, '\n'); nl >= 0 {
		meta.Summary = msg[:nl]
	}
	return meta, parents, msg, nil
}

// parseIdentity parses "Name <email> <unixtime> <tz>" into its parts.
func parseIdentity(s string) (name, email string, when int64) {
	lt := strings.IndexByte(s, '<')
	gt := strings.IndexByte(s, '>')
	if lt >= 0 && gt > lt {
		name = strings.TrimSpace(s[:lt])
		email = s[lt+1 : gt]
		rest := strings.Fields(strings.TrimSpace(s[gt+1:]))
		if len(rest) >= 1 {
			if n, err := strconv.ParseInt(rest[0], 10, 64); err == nil {
				when = n
			}
		}
	}
	return name, email, when
}

// parseUnifiedDiff parses `git diff-tree -p` output into per-file diffs. It
// enforces maxDiffFiles (commit-level Truncated) and maxDiffLinesPerFile
// (per-file TooLarge). Binary files are flagged with no hunks.
func parseUnifiedDiff(raw []byte) ([]browsemodel.FileDiff, bool) {
	lines := strings.Split(string(raw), "\n")
	var files []browsemodel.FileDiff
	var cur *browsemodel.FileDiff
	var curHunk *browsemodel.Hunk
	truncated := false

	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.Hunks = append(cur.Hunks, *curHunk)
			curHunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			flushFile()
			if len(files) >= maxDiffFiles {
				truncated = true
				return files, truncated
			}
			cur = &browsemodel.FileDiff{Status: "M"}
			// Pre-populate NewPath from "diff --git a/<path> b/<path>".
			// The +++ line may not appear for binary files, so we parse it here
			// as a fallback. It will be overwritten by +++ if present.
			rest := strings.TrimPrefix(ln, "diff --git ")
			// rest is "a/<path> b/<path>"; find the b/ half by splitting on " b/"
			if bi := strings.Index(rest, " b/"); bi >= 0 {
				cur.NewPath = rest[bi+3:]
			}
		case cur == nil:
			// preamble before first file header; ignore
		case strings.HasPrefix(ln, "new file"):
			cur.Status = "A"
		case strings.HasPrefix(ln, "deleted file"):
			cur.Status = "D"
		case strings.HasPrefix(ln, "rename from "):
			cur.Status = "R"
			cur.OldPath = strings.TrimPrefix(ln, "rename from ")
		case strings.HasPrefix(ln, "rename to "):
			cur.NewPath = strings.TrimPrefix(ln, "rename to ")
		case strings.HasPrefix(ln, "copy from "):
			cur.Status = "C"
			cur.OldPath = strings.TrimPrefix(ln, "copy from ")
		case strings.HasPrefix(ln, "copy to "):
			cur.NewPath = strings.TrimPrefix(ln, "copy to ")
		case strings.HasPrefix(ln, "Binary files "):
			cur.Binary = true
		case strings.HasPrefix(ln, "--- "):
			p := strings.TrimPrefix(ln, "--- ")
			if p != "/dev/null" {
				cur.OldPath = strings.TrimPrefix(p, "a/")
			}
		case strings.HasPrefix(ln, "+++ "):
			p := strings.TrimPrefix(ln, "+++ ")
			if p != "/dev/null" {
				cur.NewPath = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(ln, "@@"):
			flushHunk()
			if cur.TooLarge {
				continue
			}
			curHunk = &browsemodel.Hunk{Header: ln}
		case strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-") || strings.HasPrefix(ln, " "):
			// Hunk content. We are inside a hunk iff curHunk != nil; once a file
			// is TooLarge its hunks are dropped but +/- counting continues so the
			// rendered (+X −Y) totals stay accurate.
			if curHunk == nil && !cur.TooLarge {
				continue // stray content outside any hunk
			}
			kind := ln[0]
			switch kind {
			case '+':
				cur.Additions++
			case '-':
				cur.Deletions++
			}
			if cur.TooLarge {
				continue
			}
			if cur.Additions+cur.Deletions >= maxDiffLinesPerFile {
				cur.TooLarge = true
				cur.Hunks = nil
				curHunk = nil
				continue
			}
			curHunk.Lines = append(curHunk.Lines, browsemodel.DiffLine{Kind: kind, Text: ln[1:]})
		}
	}
	flushFile()

	// Ensure NewPath is populated for non-renamed files (fall back to OldPath).
	for i := range files {
		if files[i].NewPath == "" {
			files[i].NewPath = files[i].OldPath
		}
	}
	return files, truncated
}
