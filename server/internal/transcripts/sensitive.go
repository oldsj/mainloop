package transcripts

import (
	"path"
	"regexp"
	"strings"
)

// A tool call whose result is withheld entirely, not just redacted: it reads a file that holds
// credentials, or runs a command that prints them. Redaction only recognises credential shapes, and
// a credential file's contents need not have one (a kubeconfig's base64 data, an unlabeled key).
//
// The rules look at every string in the call's arguments (a Read's file_path, a Bash or shell
// command), split into words. A source file that merely has "secret" or "credential" in its name
// (internal/credentials/store.go) is not withheld: its output is redacted like any other.

// credentialCommands print credentials.
var credentialCommands = regexp.MustCompile(`(?i)` +
	`\bkubectl\b[^|;&]*\b(?:get|describe|edit)\b[^|;&]*\bsecrets?\b` +
	`|\bgh\s+auth\s+token\b` +
	`|\bop\s+(?:read|item\s+get)\b` +
	`|\bsecurity\s+find-(?:generic|internet)-password\b` +
	`|\bvault\s+(?:read|kv\s+get)\b` +
	`|\baws\s+secretsmanager\s+get-secret-value\b` +
	`|\bgcloud\s+secrets\s+versions\s+access\b` +
	`|\bgit\s+credential\b`)

// wordSplit separates the words of a command or argument.
var wordSplit = regexp.MustCompile(`[\s'"=;|&<>()` + "`" + `,]+`)

// credentialDirs are directories whose files hold credentials.
var credentialDirs = map[string]bool{".ssh": true, ".aws": true, ".kube": true, ".gnupg": true, ".docker": true}

// credentialFiles are file names that hold credentials.
var credentialFiles = map[string]bool{
	".netrc": true, ".git-credentials": true, ".npmrc": true, ".pypirc": true, "credentials": true,
	"credentials.json": true, "kubeconfig": true, "auth.json": true,
}

// dataExts are extensions of configuration or data files, where a "secret" in the name means the
// file holds one; a source file named so does not.
var dataExts = map[string]bool{"": true, ".yaml": true, ".yml": true, ".json": true, ".env": true, ".txt": true, ".toml": true, ".ini": true}

// sensitiveCall reports whether a tool call's arguments name a credential file or command.
func sensitiveCall(args any) bool {
	sensitive := false
	walkStrings(args, func(s string) {
		if sensitive {
			return
		}
		if credentialCommands.MatchString(s) {
			sensitive = true
			return
		}
		for _, w := range wordSplit.Split(s, -1) {
			if credentialPath(w) {
				sensitive = true
				return
			}
		}
	})
	return sensitive
}

// credentialPath reports whether p names a credential file or a file in a credential directory.
func credentialPath(p string) bool {
	if p == "" {
		return false
	}
	segs := strings.Split(strings.ToLower(p), "/")
	for _, s := range segs[:len(segs)-1] {
		if credentialDirs[s] {
			return true
		}
	}
	base := segs[len(segs)-1]
	ext := path.Ext(base)
	switch {
	case base == "" || base == "." || base == "..":
		return false
	case strings.HasPrefix(base, ".env"), credentialFiles[base], credentialDirs[base]:
		return true
	case strings.HasPrefix(base, "id_rsa"), strings.HasPrefix(base, "id_ed25519"), strings.HasPrefix(base, "id_ecdsa"):
		return true
	case ext == ".pem" || ext == ".key" || ext == ".p12" || ext == ".pfx":
		return true
	case (strings.Contains(base, "secret") || strings.Contains(base, "credential")) && dataExts[ext]:
		// A bare word ("grep -r secret .", a commit message) is not a file; a name without an
		// extension counts only as part of a path.
		return ext != "" || len(segs) > 1
	}
	return false
}

func walkStrings(v any, f func(string)) {
	switch x := v.(type) {
	case string:
		f(x)
	case []any:
		for _, e := range x {
			walkStrings(e, f)
		}
	case map[string]any:
		for _, e := range x {
			walkStrings(e, f)
		}
	}
}
