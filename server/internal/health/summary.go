package health

import (
	"slices"
)

// AggregationVersion identifies the rules Summarize applies. A stored summary records the version
// that built it.
const AggregationVersion = 2

// Summary is the rollup of one turn: counts and token totals, never any text.
type Summary struct {
	Failures              int            `json:"failures"`
	Categories            map[string]int `json:"categories"`
	RepeatedFailureGroups int            `json:"repeated_failure_groups"`
	UnresolvedCalls       int            `json:"unresolved_calls"`
	FrictionCoverage      string         `json:"friction_coverage"`
	InputTokens           *int64         `json:"input_tokens"`
	OutputTokens          *int64         `json:"output_tokens"`
	UsageCoverage         string         `json:"usage_coverage"`
	Reorientation         Reorientation  `json:"reorientation"`
	Inclusion             string         `json:"inclusion"`
	ChildAttribution      string         `json:"child_attribution"`
}

// Reorientation counts how much a turn re-read before it worked. A read or edit that failed is not
// counted. It carries no path: summaries outlive the observations they came from.
type Reorientation struct {
	// Turns is 1 for a turn; EditedTurns is 1 when it made an edit. Totals sum them, so
	// ReadsBeforeFirstEdit / EditedTurns is an average.
	Turns       int `json:"turns"`
	EditedTurns int `json:"edited_turns"`
	Reads       int `json:"reads"`
	// RereadCalls are the read calls of a path the same turn had already read.
	RereadCalls int `json:"reread_calls"`
	// RereadsFromEarlierTurns are the paths this turn read that an earlier turn in the same native
	// context (the same kagent session) had read or edited.
	RereadsFromEarlierTurns int `json:"rereads_from_earlier_turns"`
	Edits                   int `json:"edits"`
	// ReadsBeforeFirstEdit is null when the turn made no edit.
	ReadsBeforeFirstEdit *int `json:"reads_before_first_edit"`
}

// repeatedFailure is how many times the same failure must happen in a turn to count as a group.
const repeatedFailure = 3

type failureGroup struct {
	operation, action, path, category string
	hasPath                           bool
}

// Summarize rolls the events of one turn up. It is a pure function of the events, so a summary can
// always be rebuilt from them. The turn is complete only when the first event starts it, the last
// ends it with complete coverage, every sequence number from 1 to the last is present, and every
// tool call has exactly one start and one result that agree. Anything else is partial, and a
// partial turn never claims complete usage. earlier holds the paths the earlier turns of the same
// native context read or edited (nil when there are none); it only feeds Reorientation.RereadsFromEarlierTurns.
func Summarize(events []Event, earlier map[string]bool) Summary {
	events = slices.Clone(events)
	slices.SortFunc(events, func(a, b Event) int { return int(a.Sequence - b.Sequence) })

	starts := map[string]*Tool{}
	results := map[string]*Tool{}
	var boundary []*Turn
	var usage []*Usage
	for i := range events {
		switch {
		case events[i].Turn != nil:
			boundary = append(boundary, events[i].Turn)
		case events[i].Usage != nil:
			usage = append(usage, events[i].Usage)
		}
	}

	complete := len(boundary) > 0 && len(events) > 0 &&
		boundary[0].State == "started" &&
		(boundary[len(boundary)-1].State == "finished" || boundary[len(boundary)-1].State == "failed") &&
		boundary[len(boundary)-1].Coverage == "complete"
	if complete {
		for i, e := range events {
			if e.Sequence != int64(i+1) {
				complete = false
				break
			}
		}
	}

	for i := range events {
		t := events[i].Tool
		if t == nil {
			continue
		}
		if t.Phase == "started" {
			if _, dup := starts[t.CallKey]; dup {
				complete = false
			}
			starts[t.CallKey] = t
		} else {
			if _, dup := results[t.CallKey]; dup {
				complete = false
			}
			results[t.CallKey] = t
		}
	}

	failures := 0
	categories := map[string]int{}
	groups := map[failureGroup]int{}
	for _, t := range results {
		if t.Outcome != "error" && t.Outcome != "denied" {
			continue
		}
		failures++
		categories[t.Category]++
		g := failureGroup{operation: t.OperationKey, action: t.Action, category: t.Category}
		if t.Path != nil {
			g.hasPath, g.path = true, *t.Path
		}
		groups[g]++
	}
	repeated := 0
	for _, n := range groups {
		if n >= repeatedFailure {
			repeated++
		}
	}

	unresolved := 0
	for key, start := range starts {
		result, ok := results[key]
		if !ok {
			unresolved++
			continue
		}
		if start.ToolClass != result.ToolClass || start.OperationKey != result.OperationKey ||
			start.Action != result.Action || !samePath(start.Path, result.Path) {
			complete = false
		}
	}
	if unresolved > 0 {
		complete = false
	}
	for key := range results {
		if _, ok := starts[key]; !ok {
			complete = false
		}
	}

	// The latest revision replaces the earlier ones: usage is a running turn total, so adding the
	// revisions (or a child's inclusive total) would count tokens twice.
	var latest *Usage
	for _, u := range usage {
		if latest == nil || u.Revision > latest.Revision {
			latest = u
		}
	}
	if len(usage) > 0 {
		revisions := make([]int64, len(usage))
		for i, u := range usage {
			revisions[i] = u.Revision
		}
		slices.Sort(revisions)
		for i, r := range revisions {
			if r != int64(i+1) {
				complete = false
				break
			}
		}
	}

	s := Summary{
		Reorientation: reorientation(events, results, earlier),
		Failures:      failures, Categories: categories, RepeatedFailureGroups: repeated, UnresolvedCalls: unresolved,
		FrictionCoverage: "partial", UsageCoverage: "unknown", Inclusion: "tree_inclusive", ChildAttribution: "unavailable",
	}
	if complete {
		s.FrictionCoverage = "complete"
	}
	if latest != nil {
		s.InputTokens, s.OutputTokens = latest.InputTokens, latest.OutputTokens
		s.UsageCoverage = "partial"
		if complete && latest.Coverage == "complete" && latest.InputTokens != nil && latest.OutputTokens != nil {
			s.UsageCoverage = "complete"
		}
	}
	return s
}

// reorientation counts the turn's reads and edits from the started phase of each call, in order. A
// call whose result is an error or a denial did not read or edit anything; a call with no result
// yet counts, because the turn may still be running.
func reorientation(events []Event, results map[string]*Tool, earlier map[string]bool) Reorientation {
	r := Reorientation{Turns: 1}
	read := map[string]bool{}
	for i := range events {
		t := events[i].Tool
		if t == nil || t.Phase != "started" || (t.Action != "read" && t.Action != "edit") {
			continue
		}
		if res := results[t.CallKey]; res != nil && (res.Outcome == "error" || res.Outcome == "denied") {
			continue
		}
		if t.Action == "edit" {
			r.Edits++
			if r.ReadsBeforeFirstEdit == nil {
				r.EditedTurns = 1
				n := r.Reads
				r.ReadsBeforeFirstEdit = &n
			}
			continue
		}
		r.Reads++
		if t.Path == nil {
			continue
		}
		switch {
		case read[*t.Path]:
			r.RereadCalls++
		case earlier[*t.Path]:
			r.RereadsFromEarlierTurns++
		}
		read[*t.Path] = true
	}
	return r
}

func samePath(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
