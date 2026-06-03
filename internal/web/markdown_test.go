package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdown_SanitizesScript(t *testing.T) {
	out := renderMarkdown([]byte("# Hi\n\n<script>alert(1)</script>\n\n**bold**"))
	s := string(out)
	if strings.Contains(s, "<script") {
		t.Fatalf("script not sanitized: %s", s)
	}
	if !strings.Contains(s, "<strong>") && !strings.Contains(s, "<h1") {
		t.Fatalf("markdown not rendered: %s", s)
	}
}

func TestRenderMarkdown_NeutralizesJavascriptLink(t *testing.T) {
	out := string(renderMarkdown([]byte("[click](javascript:alert(1))")))
	if strings.Contains(out, "javascript:") {
		t.Fatalf("javascript: scheme survived: %s", out)
	}
}

func TestRenderMarkdown_StripsEventHandlers(t *testing.T) {
	out := string(renderMarkdown([]byte("<img src=x onerror=alert(1)>")))
	if strings.Contains(out, "onerror") {
		t.Fatalf("event handler survived: %s", out)
	}
}
