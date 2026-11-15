package redact

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
		out := line(in)
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
	if got := line("  a\n\tb   c  "); got != "a b c" {
		t.Errorf("%q", got)
	}
	long := line(strings.Repeat("word ", 200))
	if r := []rune(long); len(r) != lineMax || !strings.HasSuffix(long, "…") {
		t.Errorf("%d runes: %q", len(r), long)
	}
	if line("task failed: out of memory") != "task failed: out of memory" {
		t.Error("plain text changed")
	}
	if line("   ") != "" {
		t.Error("blank not empty")
	}
}

func TestSafeDetailLeavesNoStrayBracket(t *testing.T) {
	for in, want := range map[string]string{
		`{"authorization":[abc123]}`:        `{"authorization":[redacted]}`,
		`{"password": ["a b", "c"], "x":1}`: `{"password": [redacted], "x":1}`,
		`secret=[abc] next`:                 `secret=[redacted] next`,
	} {
		if got := line(in); got != want {
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
		out := line(c[0])
		for _, s := range c[1:] {
			if strings.Contains(out, s) {
				t.Errorf("%s: %q leaks %q", name, out, s)
			}
		}
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("%s: nothing redacted in %q", name, out)
		}
	}
	if got := line("proxy said Cookie: session=abc; then failed"); !strings.HasSuffix(got, "then failed") {
		t.Errorf("text after the cookie lost: %q", got)
	}
}

const lineMax = 300

func line(text string) string { return Line(text, lineMax) }

// hexOf is n lowercase hex characters (fictional, not a real key or digest).
func hexOf(n int) string { return strings.Repeat("0123456789abcdef", n/16+1)[:n] }

func TestOpaqueHexRule(t *testing.T) {
	sha1 := hexOf(40)
	digest := strings.Repeat("fedcba9876543210", 4)
	cases := []struct {
		name, in string
		keep     bool
	}{
		{"bare 40-char hex is a git SHA-1", "merged " + sha1 + " into main", true},
		{"bare 64-char hex may be a key", "AGENT_KEY_DUMP " + digest + " end", false},
		{"sha256: digest", "image@sha256:" + digest + " pulled", true},
		{"sha256- digest", "integrity sha256-" + digest + " ok", true},
		{"other prefix on 64-char hex", "md5:" + digest + " x", false},
		{"uppercase 40-char hex", "id " + strings.ToUpper(sha1) + " x", false},
		{"41-char hex", "id " + hexOf(41) + " x", false},
		{"40 chars, not all hex", "id " + hexOf(39) + "g x", false},
		{"sha256: with too short a digest", "sha256:" + hexOf(63) + "x " + "y", false},
		{"sha256- with a longer token", "sha256-" + digest + "00 x", false},
	}
	for _, c := range cases {
		for mode, f := range map[string]func(string) string{"Text": Text, "Line": line} {
			out := f(c.in)
			if c.keep && out != c.in {
				t.Errorf("%s (%s): %q -> %q, want unchanged", c.name, mode, c.in, out)
			}
			if !c.keep && !strings.Contains(out, Redacted) {
				t.Errorf("%s (%s): %q not redacted", c.name, mode, c.in)
			}
		}
	}
	// A 64-character hex value assigned to a credential name is redacted by name, digest prefix or not.
	if out := Text("AGENT_TOKEN_KEY=" + digest); strings.Contains(out, digest) {
		t.Errorf("key leaks: %q", out)
	}
}

func TestTextKeepsLayout(t *testing.T) {
	in := "line one\n\tGITHUB_TOKEN=ghx12345678\nline three  spaced\n"
	want := "line one\n\tGITHUB_TOKEN=[redacted]\nline three  spaced\n"
	if got := Text(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	long := strings.Repeat("word ", 200)
	if Text(long) != long {
		t.Error("Text truncated or changed plain text")
	}
}

func TestTextStaysOnItsLine(t *testing.T) {
	cases := map[string]string{
		"authorization": "Authorization: Basic dXNlcjpwYXNz\nnext line kept",
		"bearer":        "Bearer abcDEF123\nnext line kept",
		"assignment":    "password: hunter2\nnext line kept",
		"cookie":        "Cookie: sid=abc123;\nnext line kept",
	}
	for name, in := range cases {
		out := Text(in)
		if !strings.HasSuffix(out, "\nnext line kept") {
			t.Errorf("%s: next line lost: %q", name, out)
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("%s: nothing redacted in %q", name, out)
		}
	}
}

func TestTextRedactsMultiLinePrivateKey(t *testing.T) {
	in := "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nQUFBQUFB\n-----END OPENSSH PRIVATE KEY-----\nafter"
	out := Text(in)
	for _, s := range []string{"b3BlbnNzaC1rZXktdjEAAAAA", "QUFBQUFB"} {
		if strings.Contains(out, s) {
			t.Errorf("%q leaks %q", out, s)
		}
	}
	if out != "before\n"+Redacted+"\nafter" {
		t.Errorf("got %q", out)
	}
	if cut := Text("x\n-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkq\nmore"); cut != "x\n"+Redacted {
		t.Errorf("cut-off key: %q", cut)
	}
}
