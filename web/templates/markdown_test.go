package templates

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderToString(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatalf("render error: %v", err)
	}
	return sb.String()
}

func TestRenderMarkdown_Heading(t *testing.T) {
	out := renderToString(t, renderMarkdown("# Hello"))
	if !strings.Contains(out, "<h1>") {
		t.Errorf("expected <h1> in output, got: %s", out)
	}
}

func TestRenderMarkdown_List(t *testing.T) {
	out := renderToString(t, renderMarkdown("- foo\n- bar"))
	if !strings.Contains(out, "<ul>") || !strings.Contains(out, "<li>") {
		t.Errorf("expected <ul><li> in output, got: %s", out)
	}
}

func TestRenderMarkdown_CodeBlock(t *testing.T) {
	out := renderToString(t, renderMarkdown("```\nfmt.Println(\"hi\")\n```"))
	if !strings.Contains(out, "<code>") {
		t.Errorf("expected <code> in output, got: %s", out)
	}
}

func TestRenderMarkdown_XSS(t *testing.T) {
	out := renderToString(t, renderMarkdown("<script>alert('xss')</script>"))
	if strings.Contains(out, "<script>") {
		t.Errorf("expected <script> to be sanitized, got: %s", out)
	}
}

func TestRenderMarkdown_Empty(t *testing.T) {
	out := renderToString(t, renderMarkdown(""))
	// Must not panic; empty or blank output is fine.
	_ = out
}

func TestRenderMarkdown_NewlineOnly(t *testing.T) {
	out := renderToString(t, renderMarkdown("\n\n"))
	_ = out
}

func TestRenderMarkdown_HardWraps(t *testing.T) {
	out := renderToString(t, renderMarkdown("line one\nline two"))
	if !strings.Contains(out, "<br") {
		t.Errorf("expected <br> for hard wrap, got: %s", out)
	}
}
