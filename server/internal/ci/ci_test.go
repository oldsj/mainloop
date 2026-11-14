package ci

import (
	"strings"
	"testing"
)

func TestRunResult(t *testing.T) {
	for _, tc := range []struct{ status, conclusion, want string }{
		{"completed", "success", Passed},
		{"completed", "skipped", Passed},
		{"completed", "neutral", Failed},
		{"completed", "failure", Failed},
		{"completed", "cancelled", Failed},
		{"completed", "timed_out", Failed},
		{"queued", "", Pending},
		{"in_progress", "", Pending},
		{"waiting", "", Pending},
		{"something_new", "", Failed},
	} {
		if got := RunResult(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("RunResult(%q, %q) = %q, want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
	for state, want := range map[string]string{"success": Passed, "pending": Pending, "failure": Failed, "error": Failed} {
		if got := StatusResult(state); got != want {
			t.Errorf("StatusResult(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestJudge(t *testing.T) {
	c := func(results ...string) []Check {
		var out []Check
		for _, r := range results {
			out = append(out, Check{Result: r})
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		checks []Check
		want   State
	}{
		{"none is no_checks, never green", nil, NoChecks},
		{"all passed", c(Passed, Passed), Green},
		{"one pending", c(Passed, Pending), Running},
		{"failed wins over pending", c(Pending, Failed, Passed), Red},
	} {
		got, counts := Judge(tc.checks)
		if got != tc.want {
			t.Errorf("%s: Judge = %q, want %q", tc.name, got, tc.want)
		}
		if counts.Passed+counts.Failed+counts.Pending != len(tc.checks) {
			t.Errorf("%s: counts %+v do not add up to %d", tc.name, counts, len(tc.checks))
		}
	}
}

func TestRefValid(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		ref  Ref
		want bool
	}{
		{Ref{PR: 1}, true},
		{Ref{SHA: sha}, true},
		{Ref{}, false},
		{Ref{PR: 1, SHA: sha}, false},
		{Ref{SHA: "abc"}, false},
		{Ref{SHA: strings.Repeat("A", 40)}, false},
		{Ref{PR: -1}, false},
	} {
		if got := tc.ref.Valid(); got != tc.want {
			t.Errorf("%+v.Valid() = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestBoundCapsAFailure(t *testing.T) {
	var ann []Annotation
	for range maxAnnotationsShown + 5 {
		ann = append(ann, Annotation{Path: "f", Message: strings.Repeat("m", maxMessage+10)})
	}
	f := bound(Failure{
		Summary: strings.Repeat("s", maxSummary+1), Text: strings.Repeat("t", maxText+1),
		LogTail: "head" + strings.Repeat("l", maxText), Annotations: ann,
	})
	if len([]rune(f.Summary)) != maxSummary || len([]rune(f.Text)) != maxText {
		t.Errorf("summary %d, text %d runes", len([]rune(f.Summary)), len([]rune(f.Text)))
	}
	if len([]rune(f.LogTail)) != maxText || strings.HasPrefix(f.LogTail, "head") {
		t.Errorf("log tail kept %d runes, starts %q; want the end", len([]rune(f.LogTail)), f.LogTail[:4])
	}
	if len(f.Annotations) != maxAnnotationsShown || len([]rune(f.Annotations[0].Message)) != maxMessage {
		t.Errorf("annotations %d, message %d runes", len(f.Annotations), len([]rune(f.Annotations[0].Message)))
	}
}

func TestRenderListsWhatIsNotGreenAndWhy(t *testing.T) {
	got := Render(Status{
		State: Red, SHA: strings.Repeat("b", 40), PR: 7, Counts: Counts{Passed: 3, Failed: 1, Pending: 1},
		Checks: []Check{
			{Name: "lint", Result: Passed, Status: "success"},
			{Name: "test", Result: Failed, Status: "failure", URL: "https://ci/test"},
			{Name: "build", Result: Pending, Status: "in_progress"},
		},
		Failures: []Failure{{
			Name: "test", Summary: "1 failed", Text: "FAIL TestX",
			Annotations: []Annotation{{Path: "a.go", StartLine: 3, Level: "failure", Title: "TestX", Message: "boom"}},
			LogTail:     "last line\n",
		}},
		FailuresOmitted: 2,
	})
	for _, want := range []string{
		"CI failed at PR #7 head bbbbbbbbbbbb: 3 passed, 1 failed, 1 pending",
		"- failed test (failure) https://ci/test",
		"- pending build (in_progress)",
		"### test\n1 failed\nFAIL TestX\n- failure a.go:3 TestX: boom",
		"log tail:\n```\nlast line\n```",
		"2 more failed check(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "lint") {
		t.Errorf("render lists a passed check:\n%s", got)
	}
}
