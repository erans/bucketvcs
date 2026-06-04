package web

import (
	"bytes"
	"html"
	"html/template"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// maxHighlightBytes caps source size eligible for syntax highlighting; larger
// text blobs render as plain escaped <pre>.
const maxHighlightBytes = 1 << 20 // 1 MiB

// chromaStyleName is the dark scheme chosen in the polish-pass design.
const chromaStyleName = "monokai"

func chromaStyle() *chroma.Style {
	if s := styles.Get(chromaStyleName); s != nil {
		return s
	}
	return styles.Fallback
}

// chromaFormatter is class-based (zero inline styles — required by the strict
// UI CSP) with line numbers in a separate table column so selection/copy
// excludes them.
func chromaFormatter() *chromahtml.Formatter {
	return chromahtml.New(
		chromahtml.WithClasses(true),
		chromahtml.WithLineNumbers(true),
		chromahtml.LineNumbersInTable(true),
		chromahtml.Standalone(false),
		chromahtml.WithLinkableLineNumbers(true, "L"),
	)
}

var (
	chromaCSSOnce  sync.Once
	chromaCSSBytes []byte
)

// chromaCSS renders the monokai stylesheet once, with overrides that blend the
// code frame into the page theme and dim the line-number column.
func chromaCSS() []byte {
	chromaCSSOnce.Do(func() {
		var b bytes.Buffer
		_ = chromaFormatter().WriteCSS(&b, chromaStyle())
		b.WriteString("\n/* bucketvcs overrides */\n")
		b.WriteString(".chroma,.chroma.dark{background-color:#111;border:1px solid #333;padding:.5rem;overflow-x:auto}\n")
		b.WriteString(".chroma .lnt,.chroma .ln,.chroma.dark .lnt,.chroma.dark .ln{color:#666;-webkit-user-select:none;user-select:none}\n")
		b.WriteString(".chroma .lnt.hl,.chroma.dark .lnt.hl,.chroma .line.hl,.chroma.dark .line.hl{background-color:rgba(143,217,143,.18)}\n")
		b.WriteString(".chroma .lnt a,.chroma.dark .lnt a{color:inherit;text-decoration:none}\n")
		b.WriteString(".chroma .lnt a:hover,.chroma.dark .lnt a:hover{color:#8fd98f}\n")
		chromaCSSBytes = b.Bytes()
	})
	return chromaCSSBytes
}

// plainPre returns an HTML-escaped <pre> block (the safe fallback).
func plainPre(src []byte) template.HTML {
	return template.HTML("<pre class=\"blob\">" + html.EscapeString(string(src)) + "</pre>")
}

// highlight returns syntax-highlighted HTML for a text blob, chosen by filename.
// Output is class-based (styled by /_ui/static/chroma.css) and escaped by the
// chroma tokeniser, so it is safe to mark template.HTML. Oversized input or any
// tokeniser/format error falls back to an HTML-escaped <pre>.
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

	it, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		return plainPre(src)
	}
	var buf bytes.Buffer
	if err := chromaFormatter().Format(&buf, chromaStyle(), it); err != nil {
		return plainPre(src)
	}
	return template.HTML(buf.String())
}
