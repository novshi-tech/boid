//go:build !linux

package api

// readAttachmentBytes on non-Linux platforms falls back to the portable
// (non-openat2) reader; see readAttachmentFilePortable in attachments.go.
func readAttachmentBytes(dir, base string) ([]byte, error) {
	return readAttachmentFilePortable(dir, base)
}
