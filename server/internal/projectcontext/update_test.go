package projectcontext_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

const header = projectcontext.UpdateMarker + " This project's context changed since your brief. " +
	"Where they differ, this replaces what your brief said.\n"

func instr(v int, text string) projectcontext.Instructions {
	return projectcontext.Instructions{ProjectID: "p1", Version: v, Text: text}
}

func TestAnUpdateSendsOnlyWhatChanged(t *testing.T) {
	long := func(extra string) string {
		var b strings.Builder
		for i := range 40 {
			b.WriteString("Rule number ")
			b.WriteString(strings.Repeat("x", i))
			b.WriteString(".\n")
		}
		return b.String() + extra
	}
	for _, c := range []struct {
		name   string
		change projectcontext.Change
		want   string
	}{
		{"nothing", projectcontext.Change{}, ""},
		{"none to none", projectcontext.Change{Instructions: &[2]projectcontext.Instructions{instr(0, ""), instr(0, "")}}, ""},
		{"small instructions go whole", projectcontext.Change{Instructions: &[2]projectcontext.Instructions{instr(1, "Old."), instr(2, "Merge with --squash.")}},
			header + "## Project instructions (owner, v2, were v1)\nMerge with --squash.\n"},
		{"first instructions go whole", projectcontext.Change{Instructions: &[2]projectcontext.Instructions{instr(0, ""), instr(1, "New.")}},
			header + "## Project instructions (owner, v1, were none)\nNew.\n"},
		{"cleared", projectcontext.Change{Instructions: &[2]projectcontext.Instructions{instr(3, "Old."), instr(4, "")}},
			header + "## Project instructions cleared (were v3)\nThe owner cleared the project instructions; none apply now.\n"},
		{"large instructions send the changed lines", projectcontext.Change{Instructions: &[2]projectcontext.Instructions{
			instr(1, long("Deploy on Fridays.\n")), instr(2, long("Never deploy on Fridays.\n\n"))}},
			header + "## Project instructions (owner, v2, were v1): changed lines\n- Deploy on Fridays.\n+ Never deploy on Fridays.\n"},
		{"large instructions reordered", projectcontext.Change{Instructions: &[2]projectcontext.Instructions{
			instr(1, long("A.\nB.\n")), instr(2, long("B.\n\nA.  \n"))}},
			header + "## Project instructions (owner, v2, were v1): changed lines\nOnly blank lines, spacing or line order changed.\n"},
		{"memory", projectcontext.Change{
			Added:   []projectcontext.Entry{{ID: "m3", Text: "The owner reviews UI on a phone."}},
			Removed: []projectcontext.Entry{{ID: "m1", Text: "Deploys go through staging."}}},
			header + "## Project memory changes\n- New: The owner reviews UI on a phone. (memory m3)\n- Removed: Deploys go through staging. (memory m1)\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.change.Render(); got != c.want {
				t.Fatalf("render =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

func TestAnUpdateIsCappedAndPointsAtProjectContext(t *testing.T) {
	const note = "… more changed: call `project_context` for the current instructions and memory.\n"
	var added []projectcontext.Entry
	for i := range 20 {
		added = append(added, projectcontext.Entry{ID: "m" + strings.Repeat("0", i), Text: strings.Repeat("é", 120)})
	}
	got := projectcontext.Change{Added: added}.Render()
	if len(got) > projectcontext.MaxUpdateBytes || !strings.HasSuffix(got, note) || !utf8.ValidString(got) {
		t.Fatalf("capped block is %d bytes, ends %q", len(got), got[max(0, len(got)-120):])
	}
	// It cuts at a whole line: every entry line left is complete.
	body := strings.TrimSuffix(got, note)
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n")[2:] {
		if !strings.HasPrefix(l, "- New: ") || !strings.HasSuffix(l, ")") {
			t.Fatalf("a cut line: %q", l)
		}
	}

	// One instructions text longer than the cap is cut inside its line, on a whole character.
	one := projectcontext.Change{Instructions: &[2]projectcontext.Instructions{instr(0, ""), instr(1, strings.Repeat("ü", 4000))}}.Render()
	if len(one) > projectcontext.MaxUpdateBytes || !strings.HasSuffix(one, "\n"+note) || !utf8.ValidString(one) {
		t.Fatalf("one long line: %d bytes, valid %v", len(one), utf8.ValidString(one))
	}
}
