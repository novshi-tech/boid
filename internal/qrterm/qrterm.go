// Package qrterm renders a QR code to a terminal writer.
// The full implementation is provided by Task 4 (Phase 1-4: internal/qrterm).
// This stub satisfies the interface until that task is merged.
package qrterm

import (
	"fmt"
	"io"
)

// Print writes a QR code representation for url to w.
func Print(url string, w io.Writer) error {
	fmt.Fprintf(w, "[QR: %s]\n", url)
	return nil
}
