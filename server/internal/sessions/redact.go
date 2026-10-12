package sessions

import (
	"regexp"
	"strings"
)

// detailMax bounds a delivery's detail. The detail is shown to the owner next to the message, and
// error text from kagent, the harness or the stack is untrusted input.
const detailMax = 300

const (
	credentialField = `(?:authorization|token|secret|password|passwd|api[_-]?key|[A-Za-z_][A-Za-z0-9_]*_(?:TOKEN|KEY|SECRET|PASSWORD))`
	// A quoted value may hold whitespace and escaped quotes (JSON or a Go %q string). A bracketed
	// value (a JSON array) goes whole, so no closing bracket is left behind.
	assignmentValue = `(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|\[[^\]]*\]|[^\s,;}\[\]][^\s,;}\]]*)`
)

var secretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	// A private key block, or the rest of the text if the block is cut off.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), "[redacted]"},
	// A Cookie or Set-Cookie header: the pairs and attributes, up to the first space that is not
	// after a semicolon.
	{regexp.MustCompile(`(?i)(\b(?:set-)?cookie["']?\s*[:=]\s*)["']?[^\s=;,"']+=[^\s;,"']*(?:;\s*(?:[^\s=;,"']+=[^\s;,"']*|httponly|secure|partitioned))*`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)(\bAuthorization["']?\s*[:=]\s*)["']?[A-Za-z][A-Za-z0-9_-]*\s+[^\s"',;}\]]+["']?`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer [redacted]"},
	{regexp.MustCompile(`(?i)(\b` + credentialField + `["']?\s*[=:]\s*)` + assignmentValue), "${1}[redacted]"},
	{regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]+@`), "${1}[redacted]@"}, // credentials in a URL
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), "[redacted]"},
	{regexp.MustCompile(`\b(?:sk|ghp|gho|ghs|ghu|github_pat|xox[a-z])[-_][A-Za-z0-9_-]{8,}`), "[redacted]"},
	{regexp.MustCompile(`\b[A-Za-z0-9_-]{40,}\b`), "[redacted]"}, // any other long opaque token
}

// safeDetail returns text as one line, bounded, with best-effort credential redaction. It returns
// "" when nothing is left.
func safeDetail(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for _, p := range secretPatterns {
		text = p.re.ReplaceAllString(text, p.with)
	}
	if r := []rune(text); len(r) > detailMax {
		text = strings.TrimRight(string(r[:detailMax-1]), " ") + "…"
	}
	return text
}
