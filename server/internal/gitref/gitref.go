// Package gitref checks branch and ref names an agent or client supplies, before they reach git.
package gitref

import "strings"

// MaxLen is the longest branch or ref name accepted.
const MaxLen = 255

// Valid applies git's check-ref-format rules to a branch or ref name. It also refuses a name
// that starts with a dash, which git would read as an option.
func Valid(s string) bool {
	if s == "" || len(s) > MaxLen || s == "@" {
		return false
	}
	if strings.ContainsAny(s, " ~^:?*[\\") || strings.Contains(s, "..") ||
		strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") ||
			strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// ValidCheckoutRef reports whether s is a ref a task may start from: a valid name that is not a
// full ref path, so it cannot name refs/heads/x or any other ref namespace.
func ValidCheckoutRef(s string) bool {
	return Valid(s) && !strings.HasPrefix(s, "refs/")
}
