package templates

import (
	"bytes"

	"github.com/a-h/templ"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var (
	mdRenderer = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
	mdSanitizer = bluemonday.UGCPolicy()
)

// renderMarkdown converts Markdown source to safe HTML.
// Output is sanitized with bluemonday's UGC policy before being passed to templ.Raw.
func renderMarkdown(src string) templ.Component {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(src), &buf); err != nil {
		return templ.Raw("<pre>" + templ.EscapeString(src) + "</pre>")
	}
	safe := mdSanitizer.SanitizeBytes(buf.Bytes())
	return templ.Raw(string(safe))
}
