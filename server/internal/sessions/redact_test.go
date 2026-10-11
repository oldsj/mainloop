package sessions

import (
	"strings"
	"testing"
)

func TestSafeDetailRedactsCredentials(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc_def-ghi"
	cases := map[string]string{
		"header":     "request failed Authorization: Basic dXNlcjpwYXNz now",
		"bearer":     "got Bearer abcDEF123._~+/= back",
		"assignment": `retry with api_key=hunter2 please`,
		"quoted":     `{"password": "pa ss\"word", "ok": 1}`,
		"env":        "GITHUB_TOKEN=ghx12345678 set",
		"url":        "fetch https://user:s3cret@host.example/x failed",
		"jwt":        "token was " + jwt,
		"prefixed":   "key sk-abcdef123456 and ghp_abcdef123456 and github_pat_abcdef123456",
		"opaque":     "blob " + strings.Repeat("A1b2", 12) + " end",
	}
	secrets := []string{"dXNlcjpwYXNz", "abcDEF123", "hunter2", "pa ss", "ghx12345678", "s3cret", jwt, "sk-abcdef123456", "ghp_abcdef123456", "github_pat_abcdef123456", strings.Repeat("A1b2", 12)}
	for name, in := range cases {
		out := safeDetail(in)
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s: %q leaks %q", name, out, s)
			}
		}
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("%s: nothing redacted in %q", name, out)
		}
	}
}

func TestSafeDetailCollapsesAndBounds(t *testing.T) {
	if got := safeDetail("  a\n\tb   c  "); got != "a b c" {
		t.Errorf("%q", got)
	}
	long := safeDetail(strings.Repeat("word ", 200))
	if r := []rune(long); len(r) != detailMax || !strings.HasSuffix(long, "…") {
		t.Errorf("%d runes: %q", len(r), long)
	}
	if safeDetail("task failed: out of memory") != "task failed: out of memory" {
		t.Error("plain text changed")
	}
	if safeDetail("   ") != "" {
		t.Error("blank not empty")
	}
}

func TestSafeDetailLeavesNoStrayBracket(t *testing.T) {
	for in, want := range map[string]string{
		`{"authorization":[abc123]}`:        `{"authorization":[redacted]}`,
		`{"password": ["a b", "c"], "x":1}`: `{"password": [redacted], "x":1}`,
		`secret=[abc] next`:                 `secret=[redacted] next`,
	} {
		if got := safeDetail(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSafeDetailRedactsCookiesAndPrivateKeys(t *testing.T) {
	cases := map[string][]string{
		"cookie":     {"proxy said Cookie: session=abc123def; theme=dark; path=/ then failed", "abc123def", "theme=dark"},
		"set-cookie": {"Set-Cookie: sid=zzTopSecret; Path=/; HttpOnly rejected", "zzTopSecret"},
		"pem": {"boot failed -----BEGIN RSA PRIVATE KEY----- MIIEowIBAAKCAQEAx0 ab12cd34 -----END RSA PRIVATE KEY----- after",
			"MIIEowIBAAKCAQEAx0", "ab12cd34"},
		"pem cut off": {"-----BEGIN PRIVATE KEY----- MIIEvQIBADANBgkq", "MIIEvQIBADANBgkq"},
	}
	for name, c := range cases {
		out := safeDetail(c[0])
		for _, s := range c[1:] {
			if strings.Contains(out, s) {
				t.Errorf("%s: %q leaks %q", name, out, s)
			}
		}
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("%s: nothing redacted in %q", name, out)
		}
	}
	if got := safeDetail("proxy said Cookie: session=abc; then failed"); !strings.HasSuffix(got, "then failed") {
		t.Errorf("text after the cookie lost: %q", got)
	}
}
