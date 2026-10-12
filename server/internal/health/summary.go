package health

import (
	"slices"
)

// AggregationVersion identifies the rules Summarize applies. A stored summary records the version
// that built it.
const AggregationVersion = 1

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
	Inclusion             string         `json:"inclusion"`
	ChildAttribution      string         `json:"child_attribution"`
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
// partial turn never claims complete usage.
func Summarize(events []Event) Summary {
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
		Failures: failures, Categories: categories, RepeatedFailureGroups: repeated, UnresolvedCalls: unresolved,
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

func samePath(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
