package web

import (
	"strings"
	"testing"
)

func TestHighlight_ClassBasedWithLineNumbers(t *testing.T) {
	out := string(highlight("main.go", []byte("package main // <x>\nfunc main() {}\n")))
	// chroma v2 appends the style's mode class to the wrapper (e.g. class="chroma dark"),
	// so match the prefix rather than the exact attribute value.
	if !strings.Contains(out, `class="chroma`) {
		t.Fatalf("expected class-based chroma output: %s", out)
	}
	if strings.Contains(out, "style=") {
		t.Fatalf("inline styles present (breaks strict CSP): %s", out)
	}
	if strings.Contains(out, "<x>") {
		t.Fatalf("content not escaped: %s", out)
	}
	if !strings.Contains(out, "lnt") {
		t.Fatalf("expected line-number markup: %s", out)
	}
}

func TestChromaCSS_NonEmpty(t *testing.T) {
	css := string(chromaCSS())
	if !strings.Contains(css, ".chroma,.chroma.dark{background-color:#111") {
		t.Fatalf("dark override missing/inert: %.200s", css)
	}
}
