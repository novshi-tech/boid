package sandbox

import "regexp"

// ansiEscapeRe matches ANSI/VT100 escape sequences emitted by terminal-aware
// programs when running on a PTY. Stripped from PTY stdout before forwarding
// so that callers (e.g. shell $(...) substitution, JSON parsers) receive clean text.
//
// Patterns covered:
//   - CSI  ESC [ {params} {final}  e.g. ESC[1;37m (color), ESC[6n (cursor query)
//   - OSC  ESC ] {data} BEL|ST    e.g. ESC]11;?BEL (background-color query)
var ansiEscapeRe = regexp.MustCompile(
	`\x1b(?:` +
		`\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]` + // CSI
		`|\][^\x07\x1b]*(?:\x07|\x1b\\)` + // OSC (BEL or ST terminated)
		`)`,
)

// stripANSIEscapes removes ANSI/VT100 escape sequences from s.
func stripANSIEscapes(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}
