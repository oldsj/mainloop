package gitref_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/gitref"
)

func TestValid(t *testing.T) {
	for _, ok := range []string{"main", "feat/x", "release-1.2", "a@b", "v1.0.0"} {
		if !gitref.Valid(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{
		"", "@", "-x", "--upload-pack=x", "a..b", "x.lock", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`,
		"a@{1}", "a//b", "/a", "a/", "a.", ".a", "a/.b", "a/b.lock", "a\x00b", "a\x7fb", strings.Repeat("a", 256),
	} {
		if gitref.Valid(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidCheckoutRef(t *testing.T) {
	if !gitref.ValidCheckoutRef("feat/x") || !gitref.ValidCheckoutRef("abc123") {
		t.Fatal("ordinary ref refused")
	}
	for _, bad := range []string{"refs/heads/main", "refs/tags/v1", "-x"} {
		if gitref.ValidCheckoutRef(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
