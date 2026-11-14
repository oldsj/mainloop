package ci

import (
	"fmt"
	"strings"
)

// Render writes a status as the text an agent reads: the verdict line, the checks that are not
// green, and each detailed failure's output and annotations. Passed checks are counted, not listed.
func Render(st Status) string {
	var b strings.Builder
	short := st.SHA
	if len(short) > 12 {
		short = short[:12]
	}
	at := short
	if st.PR > 0 {
		at = fmt.Sprintf("PR #%d head %s", st.PR, short)
	}
	fmt.Fprintf(&b, "CI %s at %s: %d passed, %d failed, %d pending\n",
		st.State, at, st.Counts.Passed, st.Counts.Failed, st.Counts.Pending)
	for _, c := range st.Checks {
		if c.Result == Passed {
			continue
		}
		fmt.Fprintf(&b, "- %s %s (%s)", c.Result, c.Name, c.Status)
		if c.URL != "" {
			fmt.Fprintf(&b, " %s", c.URL)
		}
		b.WriteByte('\n')
	}
	for _, f := range st.Failures {
		fmt.Fprintf(&b, "\n### %s\n", f.Name)
		if f.Summary != "" {
			b.WriteString(strings.TrimSpace(f.Summary) + "\n")
		}
		if f.Text != "" {
			b.WriteString(strings.TrimSpace(f.Text) + "\n")
		}
		for _, a := range f.Annotations {
			loc := a.Path
			if a.StartLine > 0 {
				loc = fmt.Sprintf("%s:%d", a.Path, a.StartLine)
			}
			msg := a.Message
			if a.Title != "" {
				msg = a.Title + ": " + msg
			}
			fmt.Fprintf(&b, "- %s %s %s\n", a.Level, loc, msg)
		}
		if f.LogTail != "" {
			fmt.Fprintf(&b, "log tail:\n```\n%s\n```\n", strings.TrimRight(f.LogTail, "\n"))
		}
	}
	if st.FailuresOmitted > 0 {
		fmt.Fprintf(&b, "\n%d more failed check(s) listed above without detail.\n", st.FailuresOmitted)
	}
	return strings.TrimRight(b.String(), "\n")
}
