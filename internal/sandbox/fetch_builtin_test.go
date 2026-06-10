package sandbox

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- isPrivateIP ----------

func TestIsPrivateIP_PrivateRanges(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"127.255.255.255", true},
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"169.254.0.1", true},
		{"169.254.169.254", true}, // AWS/GCP metadata endpoint
		{"0.0.0.1", true},
		{"::1", true},
		// IPv4-mapped IPv6
		{"::ffff:127.0.0.1", true},
		{"::ffff:192.168.1.1", true},
		// Public addresses
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP %q", c.ip)
		}
		got := isPrivateIP(ip)
		if got != c.private {
			t.Errorf("isPrivateIP(%s) = %v, want %v", c.ip, got, c.private)
		}
	}
}

// ---------- htmlToMarkdown ----------

func TestHTMLToMarkdown_Headings(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"<h1>Title</h1>", "# Title"},
		{"<h2>Sub</h2>", "## Sub"},
		{"<h3>Deep</h3>", "### Deep"},
		{"<h6>Leaf</h6>", "###### Leaf"},
	}
	for _, c := range cases {
		got := htmlToMarkdown(c.input)
		if !strings.Contains(got, c.want) {
			t.Errorf("htmlToMarkdown(%q): want %q in output, got %q", c.input, c.want, got)
		}
	}
}

func TestHTMLToMarkdown_Paragraph(t *testing.T) {
	got := htmlToMarkdown("<p>Hello world.</p>")
	if !strings.Contains(got, "Hello world.") {
		t.Errorf("expected paragraph text in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_Link(t *testing.T) {
	got := htmlToMarkdown(`<a href="https://example.com">Click</a>`)
	if !strings.Contains(got, "[Click](https://example.com)") {
		t.Errorf("expected markdown link in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_UnorderedList(t *testing.T) {
	got := htmlToMarkdown("<ul><li>Alpha</li><li>Beta</li></ul>")
	if !strings.Contains(got, "- Alpha") || !strings.Contains(got, "- Beta") {
		t.Errorf("expected list items in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_OrderedList(t *testing.T) {
	got := htmlToMarkdown("<ol><li>First</li><li>Second</li></ol>")
	if !strings.Contains(got, "1. First") || !strings.Contains(got, "2. Second") {
		t.Errorf("expected ordered items in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_CodeBlock(t *testing.T) {
	got := htmlToMarkdown("<pre><code>fmt.Println()</code></pre>")
	if !strings.Contains(got, "```") || !strings.Contains(got, "fmt.Println()") {
		t.Errorf("expected code block in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_InlineCode(t *testing.T) {
	got := htmlToMarkdown("<p>Use <code>go build</code> to compile.</p>")
	if !strings.Contains(got, "`go build`") {
		t.Errorf("expected inline code in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_Bold(t *testing.T) {
	got := htmlToMarkdown("<p><strong>important</strong></p>")
	if !strings.Contains(got, "**important**") {
		t.Errorf("expected bold in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_SkipsScript(t *testing.T) {
	got := htmlToMarkdown("<html><body><p>Text</p><script>alert(1)</script></body></html>")
	if strings.Contains(got, "alert") {
		t.Errorf("script content should not appear in output, got: %q", got)
	}
	if !strings.Contains(got, "Text") {
		t.Errorf("visible text should appear in output, got: %q", got)
	}
}

func TestHTMLToMarkdown_SkipsStyle(t *testing.T) {
	got := htmlToMarkdown("<html><head><style>body{color:red}</style></head><body><p>Visible</p></body></html>")
	if strings.Contains(got, "color:red") {
		t.Errorf("style content should not appear in output, got: %q", got)
	}
	if !strings.Contains(got, "Visible") {
		t.Errorf("visible text should appear in output, got: %q", got)
	}
}

// ---------- executeFetch via httptest.Server (SSRF guard bypassed) ----------

func withNoSSRF(t *testing.T) {
	t.Helper()
	orig := newFetchClient
	t.Cleanup(func() { newFetchClient = orig })
	newFetchClient = func() *http.Client {
		return &http.Client{Timeout: fetchTimeout}
	}
}

func TestExecuteFetch_HTMLResponse(t *testing.T) {
	withNoSSRF(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET request, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h1>Hello</h1><p>World</p></body></html>")
	}))
	defer srv.Close()

	resp := executeFetch(&FetchRequest{URL: srv.URL})
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "# Hello") {
		t.Errorf("expected markdown heading in output, got: %q", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "World") {
		t.Errorf("expected paragraph text in output, got: %q", resp.Stdout)
	}
}

func TestExecuteFetch_NonHTMLPassthrough(t *testing.T) {
	withNoSSRF(t)
	body := `{"key": "value"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	resp := executeFetch(&FetchRequest{URL: srv.URL})
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != body {
		t.Errorf("expected raw body %q, got %q", body, resp.Stdout)
	}
}

func TestExecuteFetch_TextPlainPassthrough(t *testing.T) {
	withNoSSRF(t)
	body := "plain text content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	resp := executeFetch(&FetchRequest{URL: srv.URL})
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != body {
		t.Errorf("expected raw body %q, got %q", body, resp.Stdout)
	}
}

func TestExecuteFetch_SizeLimit(t *testing.T) {
	withNoSSRF(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// Write more than fetchMaxBodySize bytes
		chunk := strings.Repeat("x", 1024)
		for i := 0; i < (int(fetchMaxBodySize)/1024)+2; i++ {
			fmt.Fprint(w, chunk)
		}
	}))
	defer srv.Close()

	resp := executeFetch(&FetchRequest{URL: srv.URL})
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", resp.ExitCode, resp.Stderr)
	}
	if int64(len(resp.Stdout)) > fetchMaxBodySize+200 {
		t.Errorf("response not truncated: len=%d", len(resp.Stdout))
	}
	if !strings.Contains(resp.Stdout, "truncated") {
		t.Errorf("expected truncation notice in output")
	}
}

func TestExecuteFetch_HTTPError(t *testing.T) {
	withNoSSRF(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	resp := executeFetch(&FetchRequest{URL: srv.URL})
	if resp.ExitCode == 0 {
		t.Errorf("expected non-zero exit for 404, got 0")
	}
	if !strings.Contains(resp.Stderr, "404") {
		t.Errorf("expected 404 in error: %s", resp.Stderr)
	}
}

func TestExecuteFetch_InvalidScheme(t *testing.T) {
	resp := executeFetch(&FetchRequest{URL: "ftp://example.com/file"})
	if resp.ExitCode == 0 {
		t.Errorf("expected non-zero exit for ftp:// URL")
	}
}

func TestExecuteFetch_EmptyURL(t *testing.T) {
	resp := executeFetch(&FetchRequest{URL: ""})
	if resp.ExitCode == 0 {
		t.Errorf("expected non-zero exit for empty URL")
	}
}

func TestExecuteFetch_GETMethodEnforced(t *testing.T) {
	withNoSSRF(t)
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	resp := executeFetch(&FetchRequest{URL: srv.URL})
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", resp.ExitCode, resp.Stderr)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET request to server, got %s", gotMethod)
	}
}

// ---------- SSRF guard via private IP ranges ----------

func TestExecuteFetch_SSRFPrivateIP(t *testing.T) {
	// Test SSRF rejection by using actual private IP addresses.
	// 127.0.0.1 is private — the SSRF guard should reject it.
	resp := executeFetch(&FetchRequest{URL: "http://127.0.0.1:9999/test"})
	if resp.ExitCode == 0 {
		t.Errorf("expected non-zero exit for loopback URL, SSRF guard should have blocked it")
	}
	if !strings.Contains(resp.Stderr, "SSRF") && !strings.Contains(resp.Stderr, "private") {
		t.Errorf("expected SSRF error message, got: %s", resp.Stderr)
	}
}
