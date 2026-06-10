package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	fetchTimeout     = 30 * time.Second
	fetchMaxBodySize = int64(5 * 1024 * 1024) // 5 MB
	fetchMaxRedirects = 5
)

// newFetchClient builds the HTTP client used by executeFetch.
// Overridable in tests to bypass SSRF guard.
var newFetchClient = defaultFetchClient

func defaultFetchClient() *http.Client {
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			DialContext: ssrfSafeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= fetchMaxRedirects {
				return fmt.Errorf("fetch: too many redirects (max %d)", fetchMaxRedirects)
			}
			return nil
		},
	}
}

// privateIPNets is the list of CIDR ranges considered private/loopback/reserved.
// SSRF guard rejects any resolved IP that falls within these ranges.
var privateIPNets []net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // loopback
		"10.0.0.0/8",     // private
		"172.16.0.0/12",  // private
		"192.168.0.0/16", // private
		"169.254.0.0/16", // link-local / cloud metadata
		"0.0.0.0/8",      // this network
		"100.64.0.0/10",  // shared address (carrier-grade NAT)
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPNets = append(privateIPNets, *n)
		}
	}
}

func isPrivateIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for i := range privateIPNets {
		if privateIPNets[i].Contains(ip) {
			return true
		}
	}
	return false
}

func ssrfSafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("fetch: invalid address %q: %w", addr, err)
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("fetch: no addresses for %q", host)
	}

	for _, ipAddr := range ips {
		if isPrivateIP(ipAddr.IP) {
			return nil, fmt.Errorf("SSRF guard: %q resolves to private address %s", host, ipAddr.IP)
		}
	}

	d := net.Dialer{}
	var lastErr error
	for _, ipAddr := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("fetch: connect to %q: %w", host, lastErr)
}

// ExecFetch performs an HTTP GET on the host (no broker involved) and returns
// the result as an ExecResponse. Used by the `boid fetch` CLI subcommand.
func ExecFetch(req *FetchRequest) *ExecResponse {
	return executeFetch(req)
}

func handleFetchBuiltin(req *ExecRequest, entry *tokenEntry) *ExecResponse {
	if !entry.hasBuiltinPolicy("fetch") {
		return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: boid fetch"}
	}
	if req.Fetch == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "fetch: request is missing"}
	}

	slog.Info("fetch builtin requested",
		"job_id", entry.Context.JobID,
		"task_id", entry.Context.TaskID,
		"url", req.Fetch.URL,
	)

	return executeFetch(req.Fetch)
}

func executeFetch(req *FetchRequest) *ExecResponse {
	if req.URL == "" {
		return &ExecResponse{ExitCode: 1, Stderr: "fetch: URL is required"}
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		return &ExecResponse{ExitCode: 1, Stderr: "fetch: only http:// and https:// URLs are supported"}
	}

	client := newFetchClient()

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, req.URL, nil)
	if err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("fetch: invalid URL: %v", err)}
	}
	httpReq.Header.Set("User-Agent", "boid-fetch/1.0")

	resp, err := client.Do(httpReq)
	if err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("fetch: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("fetch: HTTP %s", resp.Status)}
	}

	limited := io.LimitReader(resp.Body, fetchMaxBodySize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("fetch: read body: %v", err)}
	}

	truncated := int64(len(body)) > fetchMaxBodySize
	if truncated {
		body = body[:fetchMaxBodySize]
	}

	ct := resp.Header.Get("Content-Type")
	var content string
	if strings.Contains(ct, "text/html") {
		content = htmlToMarkdown(string(body))
	} else {
		content = string(body)
	}

	if truncated {
		content += "\n\n[... truncated: response exceeded size limit ...]"
	}

	return &ExecResponse{ExitCode: 0, Stdout: content}
}

// htmlToMarkdown converts an HTML document to a simple markdown representation
// using golang.org/x/net/html for parsing. The conversion is intentionally
// minimal: it extracts readable text with headings, links, lists, and code
// blocks. Perfect rendering is not the goal; readability is.
func htmlToMarkdown(htmlContent string) string {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return htmlContent
	}
	var b strings.Builder
	traverseHTML(doc, &b)
	return strings.TrimSpace(b.String())
}

func traverseHTML(n *html.Node, b *strings.Builder) {
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		return
	}
	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverseHTML(c, b)
		}
		return
	}

	tag := strings.ToLower(n.Data)
	switch tag {
	case "script", "style", "noscript":
		return
	case "head":
		return
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := tag[1] - '0'
		b.WriteString("\n")
		for i := 0; i < int(level); i++ {
			b.WriteByte('#')
		}
		b.WriteString(" ")
		traverseChildrenInline(n, b)
		b.WriteString("\n\n")
	case "p":
		b.WriteString("\n")
		traverseChildrenInline(n, b)
		b.WriteString("\n\n")
	case "br":
		b.WriteString("\n")
	case "hr":
		b.WriteString("\n---\n\n")
	case "a":
		href := attrValue(n, "href")
		var text strings.Builder
		traverseChildrenInline(n, &text)
		t := strings.TrimSpace(text.String())
		if t == "" {
			return
		}
		if href != "" && href != "#" {
			b.WriteString("[")
			b.WriteString(t)
			b.WriteString("](")
			b.WriteString(href)
			b.WriteString(")")
		} else {
			b.WriteString(t)
		}
	case "ul":
		b.WriteString("\n")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && strings.ToLower(c.Data) == "li" {
				b.WriteString("- ")
				traverseChildrenInline(c, b)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	case "ol":
		b.WriteString("\n")
		i := 1
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && strings.ToLower(c.Data) == "li" {
				fmt.Fprintf(b, "%d. ", i)
				traverseChildrenInline(c, b)
				b.WriteString("\n")
				i++
			}
		}
		b.WriteString("\n")
	case "pre":
		b.WriteString("\n```\n")
		writePreText(n, b)
		b.WriteString("\n```\n\n")
	case "code":
		// Inside <pre>, writePreText handles it; standalone <code> → inline.
		if n.Parent != nil && strings.ToLower(n.Parent.Data) == "pre" {
			traverseChildrenInline(n, b)
		} else {
			b.WriteString("`")
			traverseChildrenInline(n, b)
			b.WriteString("`")
		}
	case "strong", "b":
		b.WriteString("**")
		traverseChildrenInline(n, b)
		b.WriteString("**")
	case "em", "i":
		b.WriteString("_")
		traverseChildrenInline(n, b)
		b.WriteString("_")
	case "blockquote":
		var inner strings.Builder
		traverseChildrenInline(n, &inner)
		for _, line := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverseHTML(c, b)
		}
	}
}

// traverseChildrenInline processes children with inline-context rules
// (links, bold, code) rather than block context.
func traverseChildrenInline(n *html.Node, b *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		traverseHTML(c, b)
	}
}

// writePreText extracts raw text from a <pre> block, preserving whitespace.
func writePreText(n *html.Node, b *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		} else if c.Type == html.ElementNode {
			writePreText(c, b)
		}
	}
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
