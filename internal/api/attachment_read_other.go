//go:build !linux

package api

// readAttachmentBytes on non-Linux platforms uses the portable
// (non-openat2) fallback — see readAttachmentFilePortable's doc comment in
// attachments.go for the residual TOCTOU window this accepts. boid
// currently supports Linux only (CLAUDE.md); this file exists purely so
// internal/api still compiles if some future non-Linux tooling (e.g. a
// portable CLI build) imports this package.
func readAttachmentBytes(dir, base string) ([]byte, error) {
	return readAttachmentFilePortable(dir, base)
}
