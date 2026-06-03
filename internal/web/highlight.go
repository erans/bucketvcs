package web

import (
	"bytes"
	"html"
	"html/template"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// maxHighlightBytes caps source size eligible for syntax highlighting; larger
// text blobs render as plain escaped <pre>.
const maxHighlightBytes = 1 << 20 // 1 MiB

// plainPre returns an HTML-escaped <pre> block (the safe fallback).
func plainPre(src []byte) template.HTML {
	return template.HTML("<pre class=\"blob\">" + html.EscapeString(string(src)) + "</pre>")
}

// highlight returns syntax-highlighted HTML for a text blob, chosen by filename.
// Output is derived from chroma's escaped tokeniser output, so it is safe to mark
// template.HTML. Oversized input or any tokeniser/format error falls back to an
// HTML-escaped <pre>.
func highlight(filename string, src []byte) template.HTML {
	if len(src) > maxHighlightBytes {
		return plainPre(src)
	}
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Analyse(string(src))
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	style := styles.Get("bw")
	if style == nil {
		style = styles.Fallback
	}
	// Inline styles (WithClasses(false)) keep the output self-contained — no
	// separate stylesheet to ship. Standalone(false) emits just the <pre> block.
	formatter := chromahtml.New(chromahtml.WithClasses(false), chromahtml.Standalone(false))

	it, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		return plainPre(src)
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, it); err != nil {
		return plainPre(src)
	}
	return template.HTML(buf.String())
}
