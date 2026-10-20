package tasks_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// The web parses the report header to label the message (web/src/features/chat/childReport.ts). Both
// sides read web/src/features/chat/testdata/report_texts.json (under web/, so a change to either side
// runs both CI workflows), so changing the header here fails the web's parser test until the fixture
// and the parser follow.
func TestReportTextMatchesTheFixtureTheWebParses(t *testing.T) {
	raw, err := os.ReadFile("../../../web/src/features/chat/testdata/report_texts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name        string `json:"name"`
		TaskID      string `json:"task_id"`
		Child       bool   `json:"child"`
		Attempt     int32  `json:"attempt"`
		Outcome     string `json:"outcome"`
		Summary     string `json:"summary"`
		OwnerAction string `json:"owner_action"`
		Text        string `json:"text"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the fixture is empty")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			report := dbq.TaskReport{Outcome: dbstate.ReportOutcome(c.Outcome), Summary: c.Summary}
			if c.OwnerAction != "" {
				report.OwnerAction = &c.OwnerAction
			}
			task := dbq.Task{ID: c.TaskID}
			if c.Child {
				parent := "parent"
				task.ParentTaskID = &parent
			}
			if got := tasks.ReportText(report, task, c.Attempt); got != c.Text {
				t.Fatalf("report text = %q\nfixture     = %q", got, c.Text)
			}
		})
	}
}
