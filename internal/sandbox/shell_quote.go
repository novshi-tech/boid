package sandbox

import (
	"fmt"
	"strings"
)

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if isShellSafe(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func isShellSafe(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case strings.ContainsRune("_@%+=:,./-", rune(c)):
		default:
			return false
		}
	}
	return true
}

func dirGuard(path string) string {
	return fmt.Sprintf("-d %s", shellQuote(path))
}
