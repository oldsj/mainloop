package merge

import (
	"slices"
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
)

func TestProtectedGlobs(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"k8s/app.yaml":               {"k8s/**"},
		"k8s":                        nil,
		"src/k8s/app.yaml":           nil,
		".github/workflows/ci.yml":   {".github/**"},
		"db/migrations/001.sql":      {"**/migrations/**"},
		"migrations/001.sql":         {"**/migrations/**"},
		"migrations":                 nil,
		"a/migrations/b/c.sql":       {"**/migrations/**"},
		"K8S/app.yaml":               nil,
		"docs/readme.md":             nil,
		"k8s/migrations/001.sql":     {"k8s/**", "**/migrations/**"},
		".github/migrations/001.sql": {".github/**", "**/migrations/**"},
		"server/Dockerfile":          {"**/Dockerfile*"},
		"web/Dockerfile":             {"**/Dockerfile*"},
		"Dockerfile":                 {"**/Dockerfile*"},
		"server/Dockerfile.dev":      {"**/Dockerfile*"},
		"server/dockerfile":          nil,
		"server/DockerfileNotes.md":  {"**/Dockerfile*"},
		"k8s/Dockerfile":             {"k8s/**", "**/Dockerfile*"},
	}
	for path, want := range cases {
		if got := matchedGlobs(path); !slices.Equal(got, want) {
			t.Errorf("matchedGlobs(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestPathsThatCouldBeFooledAreRefused(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "a//b", "./a", "a/../k8s/x", "a\\b", "a\x00b", "a/./b", "../x"} {
		if validPath(bad) {
			t.Errorf("validPath(%q) = true", bad)
		}
		if _, err := protectedMatches([]FileFact{{Filename: bad, Status: "modified"}}); err == nil {
			t.Errorf("protectedMatches accepted %q", bad)
		}
	}
	if _, err := protectedMatches([]FileFact{{Filename: "a.go", Status: "renamed"}}); err == nil {
		t.Error("a rename without a source was accepted")
	}
}

func TestRenameAwayFromAProtectedPathMatches(t *testing.T) {
	t.Parallel()
	got, err := protectedMatches([]FileFact{{Filename: "docs/a.md", PreviousFilename: "k8s/a.yaml", Status: "renamed"}})
	if err != nil || len(got) != 1 || got[0].Path != "k8s/a.yaml" || got[0].Glob != "k8s/**" {
		t.Fatalf("matches = %+v, %v", got, err)
	}
}

func evidence(mergeable *bool, state string, f Facts) *Evidence {
	return &Evidence{Facts: f, PR: github.PullRequestInfo{State: state, Mergeable: mergeable}}
}

func TestJudge(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	head := "1111111111111111111111111111111111111111"
	want := Facts{PathsDigest: "d", HeadSHA: head}
	base := func(mut func(*Facts)) Facts {
		f := Facts{PathsDigest: "d", HeadSHA: head, Checks: []CheckFact{{Kind: "check_run", Result: resultPassed}}}
		if mut != nil {
			mut(&f)
		}
		return f
	}
	cases := []struct {
		name string
		ev   *Evidence
		v    verdict
		why  string
	}{
		{"ready", evidence(&yes, "open", base(nil)), verdictReady, ""},
		{"head moved", evidence(&yes, "open", base(func(f *Facts) { f.HeadSHA = "2" })), verdictSuperseded, "head_moved"},
		{"closed", evidence(&yes, "closed", base(nil)), verdictBlocked, "pull_request_not_open"},
		{"unavailable", evidence(&yes, "open", base(func(f *Facts) { f.Unavailable = []string{ReasonDraft} })), verdictBlocked, "evidence_unavailable: " + ReasonDraft},
		{"paths changed", evidence(&yes, "open", base(func(f *Facts) { f.PathsDigest = "x" })), verdictBlocked, "facts_changed"},
		{"protected path appeared", evidence(&yes, "open", base(func(f *Facts) { f.Protected = []ProtectedMatch{{Path: "k8s/a", Glob: "k8s/**"}} })), verdictBlocked, "facts_changed"},
		{"failed", evidence(&yes, "open", base(func(f *Facts) { f.Checks = append(f.Checks, CheckFact{Kind: "check_run", Result: resultFailed}) })), verdictBlocked, "checks_failed"},
		{"not mergeable", evidence(&no, "open", base(nil)), verdictBlocked, "not_mergeable"},
		{"pending", evidence(&yes, "open", base(func(f *Facts) { f.Checks = []CheckFact{{Kind: "check_run", Result: resultPending}} })), verdictWait, ""},
		{"no CI at all is not green", evidence(&yes, "open", base(func(f *Facts) { f.Checks = nil })), verdictWait, ""},
		{"required check with no result", evidence(&yes, "open", base(func(f *Facts) {
			f.Checks = append(f.Checks, CheckFact{Kind: "required_check", Context: "ci"})
		})), verdictWait, ""},
		{"mergeability unknown", evidence(nil, "open", base(nil)), verdictWait, ""},
	}
	for _, tc := range cases {
		v, why := judge(tc.ev, want, head)
		if v != tc.v || (tc.why != "" && why != tc.why) {
			t.Errorf("%s: judge = %v %q, want %v %q", tc.name, v, why, tc.v, tc.why)
		}
	}
}
