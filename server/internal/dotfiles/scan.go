package dotfiles

import (
	"regexp"
	"slices"
)

// Finding is one secret-scan match: the file and the rule. It never carries the matched text.
type Finding struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// secretRules are high-precision patterns: known credential formats, key material, credentials in
// URLs and literal credential assignments. There is no generic entropy rule, which minified
// scripts in skills would trip. A match refuses the whole export.
var secretRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private_key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})`)},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"openai_key", regexp.MustCompile(`\bsk-(?:(?:proj|svcacct|admin)-[A-Za-z0-9_-]{40,}|[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20})`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"stripe_key", regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{20,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	// user:password@ in a URL, with a literal password (not a ${VAR} or $VAR reference).
	{"url_credentials", regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^/\s:@"'<>]+:[^/\s@"'<>$]{3,}@`)},
	// An environment-style assignment of a literal to an upper-case credential name.
	{"credential_assignment", regexp.MustCompile(`(?m)^\s*(?:export\s+)?[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|APIKEY|ACCESS_KEY)\s*=\s*["']?[^\s"'$]{12,}`)},
	// A JSON or YAML credential key holding a literal.
	{"credential_value", regexp.MustCompile(`(?i)["']?\b(?:api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|secret[_-]?key|password)["']?\s*:\s*["'][^"'\s$]{12,}["']`)},
}

// Scan returns every rule that matches each file, sorted by path then rule.
func Scan(files []File) []Finding {
	var out []Finding
	for _, f := range files {
		for _, r := range secretRules {
			if r.re.Match(f.Data) {
				out = append(out, Finding{Path: f.Path, Rule: r.name})
			}
		}
	}
	slices.SortFunc(out, func(a, b Finding) int {
		if a.Path != b.Path {
			if a.Path < b.Path {
				return -1
			}
			return 1
		}
		if a.Rule < b.Rule {
			return -1
		}
		if a.Rule > b.Rule {
			return 1
		}
		return 0
	})
	return out
}
