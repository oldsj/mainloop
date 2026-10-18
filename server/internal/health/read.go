package health

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// Limits of one page.
const (
	maxTurns        = 2000
	maxSessions     = 500
	maxEventsReturn = 200
)

// Filter is what the page narrows to. Nil fields do not filter. Hours is the window, counted back
// from now.
type Filter struct {
	ProjectID *string
	TaskID    *string
	SessionID *string
	AttemptID *string
	Provider  *string
	Hours     int
}

// readOnly runs fn in a read-only repeatable-read transaction: the database itself refuses a
// write, and every query of the page sees one snapshot.
func (s *Service) readOnly(ctx context.Context, fn func(q *dbq.Queries) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a read-only transaction has nothing to commit
	return fn(dbq.New(tx))
}

func (s *Service) authorize(ctx context.Context, q *dbq.Queries, f Filter) error {
	owner := s.OwnerID
	checks := []struct {
		id  *string
		own func(id string) (bool, error)
	}{
		{f.ProjectID, func(id string) (bool, error) {
			return q.HealthOwnsProject(ctx, dbq.HealthOwnsProjectParams{ID: id, UserID: owner})
		}},
		{f.TaskID, func(id string) (bool, error) {
			return q.HealthOwnsTask(ctx, dbq.HealthOwnsTaskParams{ID: id, OwnerID: owner})
		}},
		{f.SessionID, func(id string) (bool, error) {
			return q.HealthOwnsSession(ctx, dbq.HealthOwnsSessionParams{ID: id, UserID: owner})
		}},
		{f.AttemptID, func(id string) (bool, error) {
			return q.HealthOwnsAttempt(ctx, dbq.HealthOwnsAttemptParams{ID: id, OwnerID: owner})
		}},
	}
	for _, c := range checks {
		if c.id == nil || *c.id == "" {
			continue
		}
		ok, err := c.own(*c.id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
	}
	return nil
}

type turnRec struct {
	row     api.HealthTurnRow
	summary api.HealthTurnSummary
}

type group struct {
	row      api.HealthRow
	sessions []api.HealthSession
	turns    []turnRec
}

func str(s string) *string { return &s }

func nonEmpty(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

// View is the page: one row per task (or per session that has none) with its sessions, turns,
// totals, the owner's blocked or failed reports and approvals, and what the collector could not
// see. It only reads; collecting and trimming are the sweeps' work.
func (s *Service) View(ctx context.Context, f Filter) (api.HealthView, error) {
	now := s.now()
	since := now.Add(-time.Duration(f.Hours) * time.Hour)
	view := api.HealthView{Hours: f.Hours, Rows: []api.HealthRow{}, Stops: []api.HealthStop{}, Unsupported: []api.HealthViewUnsupported{
		api.Cache, api.ExactApprovalWait, api.CodexUsage, api.NativeChildAttribution,
	}}
	view.Retention.EventsDays = int(s.EventRetention / (24 * time.Hour))
	view.Retention.SummariesDays = int(s.SummaryRetention / (24 * time.Hour))

	err := s.readOnly(ctx, func(q *dbq.Queries) error {
		if err := s.authorize(ctx, q, f); err != nil {
			return err
		}
		turns, err := q.ListHealthTurns(ctx, dbq.ListHealthTurnsParams{
			OwnerID: s.OwnerID, Since: since, ProjectID: nonEmpty(f.ProjectID), TaskID: nonEmpty(f.TaskID),
			SessionID: nonEmpty(f.SessionID), AttemptID: nonEmpty(f.AttemptID), Provider: nonEmpty(f.Provider),
		})
		if err != nil {
			return err
		}
		truncated := len(turns) > maxTurns
		turns = turns[:min(len(turns), maxTurns)]

		turnSessions := map[string]bool{}
		for _, t := range turns {
			turnSessions[t.SessionID] = true
		}
		sessions, err := q.ListHealthSessions(ctx, dbq.ListHealthSessionsParams{
			OwnerID: s.OwnerID, Since: since, SessionIds: sortedKeys(turnSessions), ProjectID: nonEmpty(f.ProjectID),
			TaskID: nonEmpty(f.TaskID), SessionID: nonEmpty(f.SessionID), AttemptID: nonEmpty(f.AttemptID), Provider: nonEmpty(f.Provider),
		})
		if err != nil {
			return err
		}
		if len(sessions) > maxSessions {
			truncated = true
			sessions = sessions[:maxSessions]
		}

		var unassigned []dbq.ListHealthUnassignedTasksRow
		if nonEmpty(f.SessionID) == nil && nonEmpty(f.AttemptID) == nil && nonEmpty(f.Provider) == nil {
			unassigned, err = q.ListHealthUnassignedTasks(ctx, dbq.ListHealthUnassignedTasksParams{
				OwnerID: s.OwnerID, Since: since, ProjectID: nonEmpty(f.ProjectID), TaskID: nonEmpty(f.TaskID),
			})
			if err != nil {
				return err
			}
			if len(unassigned) > maxSessions {
				truncated = true
				unassigned = unassigned[:maxSessions]
			}
		}

		stops, err := s.unresolvedStops(ctx, q, f)
		if err != nil {
			return err
		}
		view.Stops = stops
		stalled, err := s.stalledTurns(ctx, q, f)
		if err != nil {
			return err
		}
		view.StalledTurns = &stalled

		groups, order := s.groups(sessions, unassigned, turns)
		if err := s.decorate(ctx, q, f, since, groups, order); err != nil {
			return err
		}
		view.Partial = truncated
		for _, id := range order {
			view.Rows = append(view.Rows, groups[id].row)
		}
		return nil
	})
	if err != nil {
		return api.HealthView{}, err
	}
	return view, nil
}

// maxStops bounds the unresolved stops one read returns.
const maxStops = 200

// unresolvedStops lists the attempts whose requested stop Mainloop gave up confirming, whatever
// state their task is in: a task that has ended gets no owner card for this, so this is where it
// shows. The window does not apply (a stop stays listed until it is confirmed); the task, session
// and attempt filters do.
func (s *Service) unresolvedStops(ctx context.Context, q *dbq.Queries, f Filter) ([]api.HealthStop, error) {
	rows, err := q.ListUnresolvedStops(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	stops := []api.HealthStop{}
	for _, r := range rows {
		if t := nonEmpty(f.TaskID); t != nil && r.TaskID != *t {
			continue
		}
		if a := nonEmpty(f.AttemptID); a != nil && r.AttemptID != *a {
			continue
		}
		if sid := nonEmpty(f.SessionID); sid != nil && (r.SessionID == nil || *r.SessionID != *sid) {
			continue
		}
		stops = append(stops, api.HealthStop{
			AttemptId: r.AttemptID, TaskId: r.TaskID, TaskStatus: string(r.TaskStatus), RequestedAt: r.StopRequestedAt,
			Looks: int(r.ResolveAttempts), NativeDeleted: r.NativeDeletedAt != nil,
		})
		if len(stops) == maxStops {
			break
		}
	}
	return stops, nil
}

// stalledTurns lists the delivered turns the sessions stall sweep marked stalled: no task activity
// past their allowance. Like the stops, the window does not apply (a stall stays listed until the
// turn moves again or ends); the task, session and attempt filters do.
func (s *Service) stalledTurns(ctx context.Context, q *dbq.Queries, f Filter) ([]api.TurnStall, error) {
	rows, err := q.ListStalledTurns(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	out := []api.TurnStall{}
	for _, r := range rows {
		if t := nonEmpty(f.TaskID); t != nil && (r.ProductTaskID == nil || *r.ProductTaskID != *t) {
			continue
		}
		if a := nonEmpty(f.AttemptID); a != nil && (r.AttemptID == nil || *r.AttemptID != *a) {
			continue
		}
		if sid := nonEmpty(f.SessionID); sid != nil && r.SessionID != *sid {
			continue
		}
		out = append(out, api.TurnStall{
			MessageId: r.MessageID, SessionId: r.SessionID, TaskId: r.TaskID, MainThread: r.MainThread,
			LastActivityAt: r.LastActivityAt, StalledAt: r.StalledAt, ToolRunning: r.ToolRunning,
		})
	}
	return out, nil
}

// groups seeds one group per task, or per session that has none, in the order the page lists them:
// sessions, tasks without a session, then turns whose session the filters or a deletion left out.
func (s *Service) groups(sessions []dbq.ListHealthSessionsRow, unassigned []dbq.ListHealthUnassignedTasksRow, turns []dbq.HealthTurn) (map[string]*group, []string) {
	groups := map[string]*group{}
	var order []string
	add := func(key string, taskID, projectID, title *string) *group {
		g := groups[key]
		if g == nil {
			g = &group{row: api.HealthRow{Id: key, TaskId: taskID, ProjectId: projectID, Title: title}}
			groups[key] = g
			order = append(order, key)
		}
		return g
	}
	for _, r := range sessions {
		project := r.SessionProjectID
		if r.AttemptID != nil {
			project = r.TaskProjectID
		}
		entry := api.HealthSession{SessionId: str(r.ID), Provider: r.Provider, ProjectId: project, TaskId: r.TaskID, AttemptId: r.AttemptID, Title: str(r.Title)}
		var title *string
		key := "unassigned:" + r.ID
		switch {
		case r.TaskID != nil:
			key = *r.TaskID
			entry.TaskTitle = r.TaskTitle
			title = nonEmpty(r.TaskTitle)
			if title == nil {
				title = str(r.Title)
			}
		default:
			title = str("Unassigned")
		}
		g := add(key, r.TaskID, project, title)
		if !slices.ContainsFunc(g.sessions, func(e api.HealthSession) bool { return *e.SessionId == r.ID }) {
			g.sessions = append(g.sessions, entry)
		}
	}
	for _, t := range unassigned {
		g := add(t.ID, str(t.ID), t.ProjectID, str(t.Title))
		g.sessions = append(g.sessions, api.HealthSession{Provider: "unknown", ProjectId: t.ProjectID, TaskId: str(t.ID), TaskTitle: str(t.Title)})
	}
	for _, t := range turns {
		key := "unassigned:" + t.SessionID
		var title *string
		if t.TaskID != nil {
			key = *t.TaskID
		} else {
			title = str("Unassigned")
		}
		g := add(key, t.TaskID, t.ProjectID, title)
		g.turns = append(g.turns, newTurnRec(t))
	}
	return groups, order
}

func newTurnRec(t dbq.HealthTurn) turnRec {
	var sum api.HealthTurnSummary
	// The summary is written by this package; a row that does not parse shows as an empty rollup.
	_ = json.Unmarshal(t.Summary, &sum)
	if sum.Categories == nil {
		sum.Categories = map[string]int{}
	}
	return turnRec{summary: sum, row: api.HealthTurnRow{
		A2aTaskId: t.A2aTaskID, AggregationVersion: int(t.AggregationVersion), AttemptId: t.AttemptID, Frozen: t.Frozen,
		Gateway: t.Gateway, OccurredAt: t.OccurredAt, OwnerId: t.OwnerID, ProfileRevision: t.ProfileRevision,
		ProjectId: t.ProjectID, Provider: t.Provider, RuntimeSessionId: t.RuntimeSessionID, SessionId: t.SessionID,
		Summary: sum, TaskId: t.TaskID, TurnKey: t.TurnKey,
	}}
}

// decorate adds to each group what is read per group: totals, reports, approvals, gaps and the
// receipts expected.
func (s *Service) decorate(ctx context.Context, q *dbq.Queries, f Filter, since time.Time, groups map[string]*group, order []string) error {
	sidSet, taskSet := map[string]bool{}, map[string]bool{}
	for _, g := range groups {
		for _, e := range g.sessions {
			if e.SessionId != nil {
				sidSet[*e.SessionId] = true
			}
		}
		for _, t := range g.turns {
			sidSet[t.row.SessionId] = true
		}
		if g.row.TaskId != nil {
			taskSet[*g.row.TaskId] = true
		}
	}
	sids := sortedKeys(sidSet)

	reports, err := q.ListHealthReports(ctx, dbq.ListHealthReportsParams{TaskIds: sortedKeys(taskSet), Since: since})
	if err != nil {
		return err
	}
	approvals, err := q.ListHealthApprovals(ctx, dbq.ListHealthApprovalsParams{OwnerID: s.OwnerID, SessionIds: sids, Since: since})
	if err != nil {
		return err
	}
	receipts, err := q.ListHealthReceipts(ctx, dbq.ListHealthReceiptsParams{OwnerID: s.OwnerID, SessionIds: sids, Since: since})
	if err != nil {
		return err
	}
	expected, err := q.ListHealthExpectedReceipts(ctx, dbq.ListHealthExpectedReceiptsParams{SessionIds: sids, Since: since})
	if err != nil {
		return err
	}

	// What each session's receipts say. A receipt not collected yet (pending, or unavailable) means
	// its turn may still gain observations, so coverage stays partial until the read that closes it.
	want := map[string]int{}
	for _, e := range expected {
		want[e.SessionID] = int(e.Expected)
	}
	uncollected := map[string]bool{}
	gapsOf := map[string][]string{}
	for _, r := range receipts {
		if r.State != string(dbstate.HealthCollected) {
			uncollected[r.SessionID] = true
		}
		gapsOf[r.SessionID] = append(gapsOf[r.SessionID], r.Reasons...)
	}

	for _, id := range order {
		g := groups[id]
		own := map[string]bool{}
		for _, e := range g.sessions {
			if e.SessionId != nil {
				own[*e.SessionId] = true
			}
		}
		tasksSeen := map[string]bool{}
		g.row.Turns = make([]api.HealthTurnRow, 0, len(g.turns))
		sums := make([]api.HealthTurnSummary, 0, len(g.turns))
		for _, t := range g.turns {
			own[t.row.SessionId] = true
			tasksSeen[t.row.A2aTaskId] = true
			g.row.Turns = append(g.row.Turns, t.row)
			sums = append(sums, t.summary)
		}
		g.row.Totals = totals(sums)

		g.row.Reports = []api.HealthReport{}
		if g.row.TaskId != nil {
			for _, r := range reports {
				if r.TaskID == *g.row.TaskId && (nonEmpty(f.AttemptID) == nil || r.AttemptID == *f.AttemptID) {
					g.row.Reports = append(g.row.Reports, api.HealthReport{Id: r.ID, TaskId: r.TaskID, AttemptId: r.AttemptID, Outcome: api.HealthReportOutcome(r.Outcome)})
				}
			}
		}
		g.row.Approvals = []api.HealthApproval{}
		for _, a := range approvals {
			if own[a.SessionID] {
				g.row.Approvals = append(g.row.Approvals, api.HealthApproval{Id: a.ID, RuntimeSessionId: str(a.KagentSessionID), State: str(string(a.Availability))})
			}
		}
		gaps := map[string]bool{}
		open := false
		for sid := range own {
			for _, reason := range gapsOf[sid] {
				gaps[reason] = true
			}
			open = open || uncollected[sid]
			g.row.ExpectedReceipts += want[sid]
		}
		if len(tasksSeen) < len(g.turns) {
			gaps[GapProducerRestart] = true
		}
		g.row.Gaps = sortedKeys(gaps)
		if open || len(g.row.Gaps) > 0 || g.row.ExpectedReceipts > len(tasksSeen) {
			g.row.Totals.Coverage = api.HealthTotalsCoveragePartial
			if len(g.turns) == 0 {
				g.row.Totals.Coverage = api.HealthTotalsCoverageUnknown
			}
		}

		g.row.Sessions = make([]api.HealthSession, 0, len(g.sessions))
		for _, e := range g.sessions {
			var mine []api.HealthTurnSummary
			seen := map[string]bool{}
			for _, t := range g.turns {
				if e.SessionId != nil && t.row.SessionId == *e.SessionId {
					mine = append(mine, t.summary)
					seen[t.row.A2aTaskId] = true
				}
			}
			e.Totals = totals(mine)
			// A session's own receipts and expected turns judge its totals the same way the row's do.
			if sid := e.SessionId; sid != nil && len(mine) > 0 &&
				(uncollected[*sid] || len(gapsOf[*sid]) > 0 || want[*sid] > len(seen)) {
				e.Totals.Coverage = api.HealthTotalsCoveragePartial
			}
			g.row.Sessions = append(g.row.Sessions, e)
		}
	}
	return nil
}

// totals sums the rollups of some turns. A token total is the sum of the counts that are known and
// is null when none is; coverage is complete only when every turn is complete in both friction and
// usage.
func totals(sums []api.HealthTurnSummary) api.HealthTotals {
	t := api.HealthTotals{Turns: len(sums), Coverage: api.HealthTotalsCoverageUnknown}
	var in, out, failures, groups int
	var haveIn, haveOut bool
	var re api.HealthReorientation
	var haveRe bool
	allComplete := true
	for _, s := range sums {
		if s.InputTokens != nil {
			in += *s.InputTokens
			haveIn = true
		}
		if s.OutputTokens != nil {
			out += *s.OutputTokens
			haveOut = true
		}
		if s.InputTokens != nil && s.OutputTokens != nil {
			t.UsageObservedTurns++
		}
		if s.UsageCoverage == api.HealthTurnSummaryUsageCoverageComplete {
			t.UsageCompleteTurns++
		}
		failures += s.Failures
		groups += s.RepeatedFailureGroups
		if r := s.Reorientation; r != nil {
			haveRe = true
			re.Turns += r.Turns
			re.EditedTurns += r.EditedTurns
			re.Reads += r.Reads
			re.RereadCalls += r.RereadCalls
			re.RereadsFromEarlierTurns += r.RereadsFromEarlierTurns
			re.Edits += r.Edits
			if r.ReadsBeforeFirstEdit != nil {
				n := *r.ReadsBeforeFirstEdit
				if re.ReadsBeforeFirstEdit != nil {
					n += *re.ReadsBeforeFirstEdit
				}
				re.ReadsBeforeFirstEdit = &n
			}
		}
		t.UnresolvedCalls += s.UnresolvedCalls
		if s.FrictionCoverage != api.HealthTurnSummaryFrictionCoverageComplete || s.UsageCoverage != api.HealthTurnSummaryUsageCoverageComplete {
			allComplete = false
		}
	}
	if haveIn {
		t.InputTokens = &in
	}
	if haveOut {
		t.OutputTokens = &out
	}
	if haveRe {
		t.Reorientation = &re
	}
	if len(sums) > 0 {
		t.Failures, t.RepeatedFailureGroups = &failures, &groups
		t.Coverage = api.HealthTotalsCoveragePartial
		if allComplete {
			t.Coverage = api.HealthTotalsCoverageComplete
		}
	}
	return t
}

// Events lists the retained observations of one session inside the window, oldest first, at most
// 200. It reads only.
func (s *Service) Events(ctx context.Context, sessionID string, hours int) (api.HealthEvents, error) {
	since := s.now().Add(-time.Duration(hours) * time.Hour)
	out := api.HealthEvents{Events: []api.HealthEventRow{}}
	err := s.readOnly(ctx, func(q *dbq.Queries) error {
		if err := s.authorize(ctx, q, Filter{SessionID: &sessionID}); err != nil {
			return err
		}
		rows, err := q.ListHealthEventRows(ctx, dbq.ListHealthEventRowsParams{OwnerID: s.OwnerID, SessionID: sessionID, Since: since})
		if err != nil {
			return err
		}
		out.Partial = len(rows) > maxEventsReturn
		for _, r := range rows[:min(len(rows), maxEventsReturn)] {
			var ev api.HealthEvent
			if err := json.Unmarshal(r.Payload, &ev); err != nil {
				return fmt.Errorf("health: stored event %s: %w", r.EventID, err)
			}
			out.Events = append(out.Events, api.HealthEventRow{
				AttemptId: r.AttemptID, EventId: r.EventID, Payload: ev, ReceivedAt: r.ReceivedAt, TaskId: r.TaskID,
			})
		}
		return nil
	})
	if err != nil {
		return api.HealthEvents{}, err
	}
	return out, nil
}
