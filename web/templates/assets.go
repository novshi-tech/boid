package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/novshi-tech/boid/web"
)

// staticCSSVersion is an 8-hex-char content hash of the embedded style.css,
// computed once at process startup. It's appended as ?v=<hash> to the
// stylesheet URL in Layout so each deploy forces CDN / browser caches to
// refetch (Cloudflare in particular sets a 4h TTL by default on
// /static/*.css, which hid CSS edits until the TTL expired).
var staticCSSVersion = computeStaticCSSVersion()

func computeStaticCSSVersion() string {
	f, err := web.StaticFS.Open("static/style.css")
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// StaticCSSURL returns the stylesheet URL with a cache-busting content hash.
// Falls back to the unversioned path if the hash could not be computed —
// caching will be imperfect but the page still renders.
func StaticCSSURL() string {
	if staticCSSVersion == "" {
		return "/static/style.css"
	}
	return "/static/style.css?v=" + staticCSSVersion
}
