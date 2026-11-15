package dotfiles_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/dotfiles"
)

// Fake credentials are assembled at run time, so no secret-shaped literal is committed (and no
// scanner of this repository flags the test).
func fake(prefix string, n int, alphabet string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

const (
	alnum = "aB3dE5gH7jK9mN1pQ2"
	upper = "ABCDEFGHJKLMNPQRST234567"
)

func TestScanFindsCredentials(t *testing.T) {
	for rule, text := range map[string]string{
		"private_key":           "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3Blbg==\n",
		"github_token":          "token: " + fake("gh"+"p_", 36, alnum),
		"aws_access_key":        "id " + fake("AK"+"IA", 16, upper),
		"slack_token":           fake("xo"+"xb-", 24, alnum),
		"anthropic_key":         fake("sk-"+"ant-", 40, alnum),
		"openai_key":            fake("sk-"+"proj-", 48, alnum),
		"google_api_key":        fake("AI"+"za", 35, alnum),
		"stripe_key":            fake("sk_"+"live_", 24, alnum),
		"jwt":                   fake("ey"+"J", 16, alnum) + "." + fake("ey"+"J", 16, alnum) + "." + fake("", 16, alnum),
		"url_credentials":       "remote: https://deploy:" + fake("", 12, alnum) + "@git.example.test/x.git",
		"credential_assignment": "export SERVICE_API_TOKEN=" + fake("", 20, alnum),
		"credential_value":      `{"client_secret": "` + fake("", 20, alnum) + `"}`,
	} {
		t.Run(rule, func(t *testing.T) {
			got := dotfiles.Scan([]dotfiles.File{{Path: "skills/s/SKILL.md", Data: []byte(text)}})
			if !slices.Contains(got, dotfiles.Finding{Path: "skills/s/SKILL.md", Rule: rule}) {
				t.Fatalf("findings = %v, want %s", got, rule)
			}
		})
	}
}

func TestScanLeavesOrdinaryTextAlone(t *testing.T) {
	benign := strings.Join([]string{
		"# Owner instructions: report evidence, not claims.",
		"Use $GITHUB_TOKEN or ${API_TOKEN}; never paste a token.",
		"export API_TOKEN=$(op read op://vault/item/token)",
		"const token = requestHeaders.authorization;",
		`password: "${PASSWORD}"`,
		"git clone https://github.com/example/repo.git",
		"postgres://localhost:5432/db",
		"skill-identifier-sk-not-a-key",
		"function a(b){return b.map(c=>c*2).filter(Boolean)}" + strings.Repeat("x", 200),
		"-----BEGIN PUBLIC KEY-----",
	}, "\n")
	if got := dotfiles.Scan([]dotfiles.File{{Path: "AGENTS.md", Data: []byte(benign)}}); len(got) != 0 {
		t.Fatalf("findings = %v, want none", got)
	}
}

func TestScanReportsEveryFileAndRuleSorted(t *testing.T) {
	key := "-----BEGIN " + "RSA PRIVATE KEY-----"
	got := dotfiles.Scan([]dotfiles.File{
		{Path: "skills/b/SKILL.md", Data: []byte(key)},
		{Path: "AGENTS.md", Data: []byte(key + "\nexport DB_PASSWORD=" + fake("", 16, alnum))},
	})
	want := []dotfiles.Finding{
		{Path: "AGENTS.md", Rule: "credential_assignment"},
		{Path: "AGENTS.md", Rule: "private_key"},
		{Path: "skills/b/SKILL.md", Rule: "private_key"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("findings = %v\nwant %v", got, want)
	}
}
