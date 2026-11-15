// Package redact removes credentials from untrusted text before Mainloop stores or shows it:
// error details from kagent, the harness or the stack ([Line]), and session transcripts ([Text]).
// Redaction is best effort: it recognises common credential shapes, and a secret in a shape it
// does not know passes through.
package redact

import (
	"regexp"
	"strings"
)

// Redacted replaces every credential.
const Redacted = "[redacted]"

const (
	credentialField = `(?:authorization|token|secret|password|passwd|api[_-]?key|[A-Za-z_][A-Za-z0-9_]*_(?:TOKEN|KEY|SECRET|PASSWORD))`
	// A quoted value may hold whitespace and escaped quotes (JSON or a Go %q string). A bracketed
	// value (a JSON array) goes whole, so no closing bracket is left behind.
	assignmentValue = `(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|\[[^\]]*\]|[^\s,;}\[\]][^\s,;}\]]*)`
)

// Spacing inside a header or an assignment is [ \t], not \s, so a match in multi-line text never
// runs on into the next line.
var secretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	// A private key block, across lines, or the rest of the text if the block is cut off.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
	// A Cookie or Set-Cookie header: the pairs and attributes, up to the first space that is not
	// after a semicolon.
	{regexp.MustCompile(`(?i)(\b(?:set-)?cookie["']?[ \t]*[:=][ \t]*)["']?[^\s=;,"']+=[^\s;,"']*(?:;[ \t]*(?:[^\s=;,"']+=[^\s;,"']*|httponly|secure|partitioned))*`), "${1}" + Redacted},
	{regexp.MustCompile(`(?i)(\bAuthorization["']?[ \t]*[:=][ \t]*)["']?[A-Za-z][A-Za-z0-9_-]*[ \t]+[^\s"',;}\]]+["']?`), "${1}" + Redacted},
	{regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]+`), "Bearer " + Redacted},
	{regexp.MustCompile(`(?i)(\b` + credentialField + `["']?[ \t]*[=:][ \t]*)` + assignmentValue), "${1}" + Redacted},
	{regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Redacted + "@"}, // credentials in a URL
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), Redacted},
	{regexp.MustCompile(`\b(?:sk|ghp|gho|ghs|ghu|github_pat|xox[a-z])[-_][A-Za-z0-9_-]{8,}`), Redacted},
}

// opaqueToken is any other long opaque token. Two hex shapes are kept (see [keepOpaque]).
var opaqueToken = regexp.MustCompile(`\b[A-Za-z0-9_-]{40,}\b`)

// Text redacts credentials in text and keeps its layout: lines, spacing and length apart from the
// redactions. It is for transcripts, which are stored and searched, so it runs before anything is
// written or indexed.
func Text(text string) string {
	for _, p := range secretPatterns {
		text = p.re.ReplaceAllString(text, p.with)
	}
	return redactOpaque(text)
}

// Line returns text as one line of at most max runes, with credentials redacted. It returns ""
// when nothing is left.
func Line(text string, max int) string {
	text = Text(strings.Join(strings.Fields(text), " "))
	if r := []rune(text); len(r) > max {
		text = strings.TrimRight(string(r[:max-1]), " ") + "…"
	}
	return text
}

// redactOpaque replaces each long opaque token that [keepOpaque] does not keep.
func redactOpaque(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range opaqueToken.FindAllStringIndex(text, -1) {
		if keepOpaque(text, m[0], m[1]) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(Redacted)
		last = m[1]
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// keepOpaque reports whether the token text[start:end] is a digest rather than a credential:
//   - a bare 40-character lowercase hex string, a git SHA-1;
//   - 64 lowercase hex characters right after "sha256:" or "sha256-", an image or content digest.
//
// A bare 64-character hex string is redacted: a 32-byte key such as AGENT_TOKEN_KEY has exactly
// that shape.
func keepOpaque(text string, start, end int) bool {
	tok := text[start:end]
	switch {
	case len(tok) == 40 && isHex(tok):
		return true
	case len(tok) == 64 && isHex(tok):
		return strings.HasSuffix(text[:start], "sha256:")
	case len(tok) == 7+64 && strings.HasPrefix(tok, "sha256-") && isHex(tok[7:]):
		return true
	}
	return false
}

func isHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
