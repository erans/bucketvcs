package web

import (
	"bytes"
	"context"
	"html/template"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// isMarkdownPath reports whether a blob path renders as Markdown.
func isMarkdownPath(p string) bool {
	l := strings.ToLower(p)
	return strings.HasSuffix(l, ".md") || strings.HasSuffix(l, ".markdown")
}

// ugcPolicy is the HTML sanitization policy for rendered Markdown. Built once;
// bluemonday policies are safe for concurrent use after construction.
var ugcPolicy = bluemonday.UGCPolicy()

// renderMarkdown converts Markdown to sanitized HTML safe to embed. goldmark
// renders to HTML; bluemonday's UGC policy then strips scripts/event handlers
// and other untrusted markup before the result is marked template.HTML.
func renderMarkdown(src []byte) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert(src, &buf); err != nil {
		return ""
	}
	clean := ugcPolicy.SanitizeBytes(buf.Bytes())
	return template.HTML(clean)
}

// renderReadme finds a root README among entries and renders it. Markdown files
// are rendered + sanitized; returns "" when no README is present (or it is
// binary / too large to read).
func (s *server) renderReadme(ctx context.Context, br browseRoute, oid string, entries []browsemodel.TreeEntry) template.HTML {
	var name string
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		switch strings.ToLower(e.Name) {
		case "readme.md", "readme.markdown":
			name = e.Path
		}
		if name != "" {
			break
		}
	}
	if name == "" {
		return ""
	}
	b, err := s.content.ReadBlob(ctx, br.tenant, br.repo, oid, name)
	if err != nil || b.Binary || b.TooLarge {
		return ""
	}
	return renderMarkdown(b.Bytes)
}
