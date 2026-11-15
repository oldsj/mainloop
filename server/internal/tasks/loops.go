package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Loops (docs/specs/loops.md) are durable wake-ups that main or a supervisor arms instead of
// polling: a loop watches a task's status, a code task's pull request, or a timer, and each firing
// is one message to the session that armed it, delivered with child reports in the same batch.

// Loop limits.
const (
	MaxLoopNote             = 2000
	MaxArmedLoops           = 20
	DefaultLoopMaxFirings   = 20
	MaxLoopFirings          = 100
	DefaultLoopMinInterval  = 60 * time.Second
	MaxLoopMinInterval      = 24 * time.Hour
	DefaultLoopExpiry       = 24 * time.Hour
	MaxLoopExpiry           = 7 * 24 * time.Hour
	MaxLoopMinutes          = 7 * 24 * 60
	maxLoopFacts            = 10
	maxLoopFactTitle        = 120
	maxListedLoops          = 50
	defaultLoopPoll         = 15 * time.Second
	loopMaxWait             = 5 * time.Minute
	ReasonLoopLimit         = "loop_limit"
	ReasonLoopNotFound      = "loop_not_found"
	ReasonLoopSubjectDenied = "loop_subject_denied"
	ReasonLoopNeedsCodeTask = "loop_requires_code_task"
)

// LoopChannel wakes the loop runner: a subject task's status change notifies it (migration 00590).
const LoopChannel = "mainloop_task_loops"

// LoopTable is the table of loops the runner evaluates.
func LoopTable() reconcile.Table {
	return reconcile.NewTable("task_loops", string(dbstate.LoopArmed))
}

// LoopLoop evaluates armed loops: it fires a task loop when its subject's status changes and a time
// loop when its timer is due, and ends loops that expired or whose recipient ended.
func (s *Service) LoopLoop() reconcile.Loop {
	return reconcile.Loop{
		Name: "task-loops", Table: LoopTable(), Handle: s.handleLoop,
		Interval: s.Limits.Interval, Wake: []string{LoopChannel},
	}
}

// LoopInput arms a loop.
type LoopInput struct {
	RequestID string
	// TaskID is the subject: required for a task trigger, optional for a time trigger.
	TaskID  string
	Trigger dbstate.LoopTrigger
	// Statuses limit a task trigger to these statuses; empty fires on any status or reason change.
	Statuses []dbstate.TaskStatus
	// PRConditions limit a pull_request trigger to changes after which one of them holds; empty
	// fires on any change of the pull request's number, state or CI result.
	PRConditions []dbstate.PRCondition
	// AfterMinutes is a time trigger's first firing; EveryMinutes its repeat (0 fires once). With
	// only EveryMinutes, the first firing is one interval away.
	AfterMinutes int
	EveryMinutes int
	Note         string
	// Zero takes the default.
	MaxFirings     int
	MinInterval    *time.Duration
	ExpiresMinutes int
}

// loopRecipient is the delivery key of the loops a principal arms: main's or the supervisor's own
// task. Children and the owner do not arm loops.
func loopRecipient(p Principal) (string, error) {
	switch {
	case p.Final:
		return "", refusal(403, ReasonManagementDenied)
	case p.Role == dbstate.RoleMain:
		return "main", nil
	case p.Role == dbstate.RoleSupervisor:
		return "parent-task:" + p.TaskID, nil
	}
	return "", refusal(403, ReasonManagementDenied)
}

func validLoop(in *LoopInput) error {
	bad := refusal(422, ReasonInvalidRequest)
	if in.RequestID == "" || len(in.RequestID) > 100 || in.Note == "" || len(in.Note) > MaxLoopNote {
		return bad
	}
	if in.MaxFirings == 0 {
		in.MaxFirings = DefaultLoopMaxFirings
	}
	if in.MaxFirings < 1 || in.MaxFirings > MaxLoopFirings {
		return bad
	}
	if in.MinInterval == nil {
		d := DefaultLoopMinInterval
		in.MinInterval = &d
	}
	if *in.MinInterval < 0 || *in.MinInterval > MaxLoopMinInterval {
		return bad
	}
	if in.ExpiresMinutes == 0 {
		in.ExpiresMinutes = int(DefaultLoopExpiry.Minutes())
	}
	if in.ExpiresMinutes < 1 || in.ExpiresMinutes > int(MaxLoopExpiry.Minutes()) {
		return bad
	}
	switch in.Trigger {
	case dbstate.LoopTriggerTask:
		if in.TaskID == "" || in.AfterMinutes != 0 || in.EveryMinutes != 0 || len(in.PRConditions) != 0 {
			return bad
		}
		for _, st := range in.Statuses {
			if !slices.Contains(taskStatuses, st) {
				return bad
			}
		}
	case dbstate.LoopTriggerPullRequest:
		if in.TaskID == "" || in.AfterMinutes != 0 || in.EveryMinutes != 0 || len(in.Statuses) != 0 {
			return bad
		}
		for _, c := range in.PRConditions {
			if !slices.Contains(dbstate.PRConditions, c) {
				return bad
			}
		}
	case dbstate.LoopTriggerTime:
		if len(in.Statuses) != 0 || len(in.PRConditions) != 0 || in.AfterMinutes < 0 || in.EveryMinutes < 0 ||
			in.AfterMinutes > MaxLoopMinutes || in.EveryMinutes > MaxLoopMinutes ||
			in.AfterMinutes == 0 && in.EveryMinutes == 0 {
			return bad
		}
	default:
		return bad
	}
	return nil
}

var taskStatuses = []dbstate.TaskStatus{
	dbstate.TaskQueued, dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskBlocked,
	dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled,
}

// Arm records a loop for the principal. The same request id returns the loop it armed; a different
// request under it is refused.
func (s *Service) Arm(ctx context.Context, p Principal, in LoopInput) (dbq.TaskLoop, error) {
	recipient, err := loopRecipient(p)
	if err != nil {
		return dbq.TaskLoop{}, err
	}
	if err := validLoop(&in); err != nil {
		return dbq.TaskLoop{}, err
	}
	statuses := make([]string, len(in.Statuses))
	for i, st := range in.Statuses {
		statuses[i] = string(st)
	}
	slices.Sort(statuses)
	statuses = slices.Compact(statuses)
	conditions := make([]string, len(in.PRConditions))
	for i, c := range in.PRConditions {
		conditions[i] = string(c)
	}
	slices.Sort(conditions)
	conditions = slices.Compact(conditions)
	var out dbq.TaskLoop
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		// One arm per recipient at a time, so the count holds for the insert.
		if err := db.XactLock(ctx, tx, "mainloop:task-loops:"+recipient); err != nil {
			return err
		}
		prior, err := q.GetTaskLoopByRequest(ctx, dbq.GetTaskLoopByRequestParams{PrincipalKey: p.Key(), RequestID: in.RequestID})
		if err == nil {
			if !sameLoop(prior, in, statuses, conditions) {
				return refusal(409, ReasonPayloadConflict)
			}
			out = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := s.now()
		var subject *dbq.Task
		if in.TaskID != "" {
			t, err := q.GetTask(ctx, dbq.GetTaskParams{ID: in.TaskID, OwnerID: p.OwnerID})
			if err != nil {
				return notFoundOr(err, ReasonTaskNotFound)
			}
			if _, manage := p.access(t); !manage {
				return refusal(403, ReasonLoopSubjectDenied)
			}
			if in.Trigger == dbstate.LoopTriggerPullRequest && t.Mode != dbstate.TaskModeCode {
				return refusal(422, ReasonLoopNeedsCodeTask)
			}
			subject = &t
		}
		n, err := q.CountArmedTaskLoops(ctx, recipient)
		if err != nil {
			return err
		}
		if n >= MaxArmedLoops {
			return refusalDetail(409, ReasonLoopLimit, map[string]any{"limit": MaxArmedLoops})
		}
		params := dbq.InsertTaskLoopParams{
			ID: uuid.NewString(), OwnerID: p.OwnerID, PrincipalKey: p.Key(), RequestID: in.RequestID,
			RecipientKey: recipient, Trigger: in.Trigger, Statuses: statuses, PrConditions: conditions, Note: in.Note,
			MaxFirings: int32(in.MaxFirings), MinIntervalSeconds: int32(in.MinInterval.Seconds()), //nolint:gosec // bounded above
			ExpiresAt: now.Add(time.Duration(in.ExpiresMinutes) * time.Minute),
		}
		if subject != nil {
			params.SubjectTaskID = &subject.ID
		}
		switch in.Trigger {
		case dbstate.LoopTriggerTask:
			// The baseline is the subject as it is now. A subject already in a status the loop
			// names has none, so the first look fires: arming after the change still hears of it.
			if len(statuses) == 0 || !wanted(statuses, subject.Status) {
				fp := fingerprint(*subject)
				params.Baseline = &fp
			}
			params.NextAttemptAt = now
		case dbstate.LoopTriggerPullRequest:
			// The same for a condition that already holds: green CI before the arm still fires.
			if len(conditions) == 0 || !prHolds(conditions, *subject) {
				fp := prFingerprint(*subject)
				params.Baseline = &fp
			}
			params.NextAttemptAt = now
		case dbstate.LoopTriggerTime:
			after := in.AfterMinutes
			if after == 0 {
				after = in.EveryMinutes
			}
			at := now.Add(time.Duration(after) * time.Minute)
			params.FireAt = &at
			params.NextAttemptAt = at
			if in.EveryMinutes > 0 {
				every := int32(in.EveryMinutes * 60) //nolint:gosec // bounded above
				params.EverySeconds = &every
			}
		}
		out, err = q.InsertTaskLoop(ctx, params)
		return err
	})
	return out, err
}

// sameLoop reports whether a stored loop is what the request asks for.
func sameLoop(l dbq.TaskLoop, in LoopInput, statuses, conditions []string) bool {
	every := 0
	if l.EverySeconds != nil {
		every = int(*l.EverySeconds) / 60
	}
	return l.Trigger == in.Trigger && deref(l.SubjectTaskID) == in.TaskID && slices.Equal(l.Statuses, statuses) &&
		slices.Equal(l.PrConditions, conditions) &&
		l.Note == in.Note && int(l.MaxFirings) == in.MaxFirings && every == in.EveryMinutes &&
		time.Duration(l.MinIntervalSeconds)*time.Second == *in.MinInterval
}

// Loops lists the principal's loops: the armed ones, then the latest that ended.
func (s *Service) Loops(ctx context.Context, p Principal) ([]dbq.TaskLoop, error) {
	recipient, err := loopRecipient(p)
	if err != nil {
		return nil, err
	}
	return dbq.New(s.Pool).ListTaskLoops(ctx, dbq.ListTaskLoopsParams{
		OwnerID: p.OwnerID, RecipientKey: recipient, MaxRows: maxListedLoops,
	})
}

// LoopFirings lists a loop's latest firings, newest first.
func (s *Service) LoopFirings(ctx context.Context, loopID string, n int) ([]dbq.ListLoopFiringsRow, error) {
	return dbq.New(s.Pool).ListLoopFirings(ctx, dbq.ListLoopFiringsParams{LoopID: loopID, MaxRows: int32(n)}) //nolint:gosec // a small count
}

// StopLoop stops one of the principal's armed loops. A firing already made is still delivered.
func (s *Service) StopLoop(ctx context.Context, p Principal, loopID string) (dbq.TaskLoop, error) {
	recipient, err := loopRecipient(p)
	if err != nil {
		return dbq.TaskLoop{}, err
	}
	var l dbq.TaskLoop
	// A transaction, so the caller's credential is checked with the write (agentauth.Guarded).
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		l, err = q.StopTaskLoop(ctx, dbq.StopTaskLoopParams{ID: loopID, OwnerID: p.OwnerID, RecipientKey: recipient})
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Not armed: a loop of the principal's that already ended is returned as it is.
		l, err = q.GetTaskLoopByID(ctx, loopID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (l.OwnerID != p.OwnerID || l.RecipientKey != recipient) {
			return refusal(404, ReasonLoopNotFound)
		}
		return err
	})
	return l, err
}

func wanted(statuses []string, st dbstate.TaskStatus) bool {
	return len(statuses) == 0 || slices.Contains(statuses, string(st))
}

// fingerprint is what a task trigger compares: the subject's status and reason.
func fingerprint(t dbq.Task) string {
	if t.Reason == nil {
		return string(t.Status)
	}
	return string(t.Status) + "/" + string(*t.Reason)
}

// prFingerprint is what a pull_request trigger compares: the pull request the projection saw, its
// state, and CI's result with the commit it is for.
func prFingerprint(t dbq.Task) string {
	if t.PrNumber == nil {
		return "none"
	}
	return fmt.Sprintf("#%d %s ci %s@%s", *t.PrNumber, t.PrState, t.CiState, deref(t.CiHeadSha))
}

// prHolds reports whether one of the conditions holds for the task's pull request. CI counts only
// when its result is for the pull request's head.
func prHolds(conditions []string, t dbq.Task) bool {
	atHead := t.PrNumber != nil && t.CiHeadSha != nil && t.PrHeadSha != nil && *t.CiHeadSha == *t.PrHeadSha
	for _, c := range conditions {
		switch dbstate.PRCondition(c) {
		case dbstate.PRCISuccess:
			if atHead && t.CiState == "success" {
				return true
			}
		case dbstate.PRCIFailure:
			if atHead && t.CiState == "failure" {
				return true
			}
		case dbstate.PRMerged:
			if t.PrState == "merged" {
				return true
			}
		case dbstate.PRClosed:
			if t.PrState == "closed" {
				return true
			}
		}
	}
	return false
}

func prFact(t dbq.Task) string {
	if t.PrNumber == nil {
		return fmt.Sprintf("task %s (%q) has no pull request", t.ID, shortTitle(t.Title))
	}
	ci := "CI " + t.CiState
	switch {
	case t.CiHeadSha == nil:
	case t.PrHeadSha != nil && *t.CiHeadSha == *t.PrHeadSha:
		ci += " at the head " + short(*t.CiHeadSha)
	default:
		ci += " at " + short(*t.CiHeadSha) + ", not yet at the head " + short(deref(t.PrHeadSha))
	}
	return fmt.Sprintf("task %s (%q) pull request #%d is %s, %s", t.ID, shortTitle(t.Title), *t.PrNumber, t.PrState, ci)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func shortTitle(title string) string {
	if r := []rune(title); len(r) > maxLoopFactTitle {
		return string(r[:maxLoopFactTitle]) + "…"
	}
	return title
}

func taskFact(t dbq.Task, was *string) string {
	fact := fmt.Sprintf("task %s (%q) is %s", t.ID, shortTitle(t.Title), fingerprint(t))
	if was != nil {
		fact += ", was " + *was
	}
	return fact
}

// appendFact adds an occurrence to a firing's facts, keeping at most maxLoopFacts: once full, the
// last is replaced, so the first ones and the latest are always there. It returns how many were
// dropped.
func appendFact(facts []string, fact string) ([]string, int) {
	if len(facts) < maxLoopFacts {
		return append(facts, fact), 0
	}
	facts[len(facts)-1] = fact
	return facts, 1
}

// loopEval is one evaluation's change to its loop.
type loopEval struct {
	state    dbstate.LoopState
	baseline *string
	fireAt   *time.Time
	firings  int32
	lastAt   *time.Time
	next     time.Time
	fact     string
}

func (s *Service) loopPoll() time.Duration {
	if s.Limits.LoopPoll > 0 {
		return s.Limits.LoopPoll
	}
	return defaultLoopPoll
}

// handleLoop evaluates one armed loop. It holds the loop row for the whole transaction and makes no
// call out: a firing is a row, delivered by the delivery loop.
func (s *Service) handleLoop(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		l, err := q.GetTaskLoopForUpdate(ctx, c.ID)
		if err != nil {
			return err
		}
		if l.State != dbstate.LoopArmed {
			// Stopped since the claim: the stop released the lease.
			return reconcile.ErrLeaseLost
		}
		e, err := s.evaluate(ctx, q, l)
		if err != nil {
			return err
		}
		if e.fact != "" {
			if err := s.fire(ctx, q, l, &e); err != nil {
				return err
			}
		}
		n, err := q.SetTaskLoopEvaluation(ctx, dbq.SetTaskLoopEvaluationParams{
			ID: l.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
			State: e.state, Baseline: e.baseline, FireAt: e.fireAt, Firings: e.firings, LastFiredAt: e.lastAt,
			NextAttemptAt: e.next,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return reconcile.ErrLeaseLost
		}
		return nil
	})
	if errors.Is(err, reconcile.ErrLeaseLost) || errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// evaluate decides what happened to a loop since it last looked.
func (s *Service) evaluate(ctx context.Context, q *dbq.Queries, l dbq.TaskLoop) (loopEval, error) {
	now := s.now()
	e := loopEval{
		state: l.State, baseline: l.Baseline, fireAt: l.FireAt, firings: l.Firings, lastAt: l.LastFiredAt,
		next: now.Add(loopMaxWait),
	}
	if parentID, ok := strings.CutPrefix(l.RecipientKey, "parent-task:"); ok {
		p, err := q.GetTaskByID(ctx, parentID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return e, err
		}
		if err != nil || TaskFinal(p.Status) {
			e.state = dbstate.LoopEnded
			return e, nil
		}
	}
	if !now.Before(l.ExpiresAt) {
		e.state = dbstate.LoopExpired
		return e, nil
	}
	switch l.Trigger {
	case dbstate.LoopTriggerTask:
		t, err := q.GetTaskByID(ctx, deref(l.SubjectTaskID))
		if errors.Is(err, pgx.ErrNoRows) {
			e.state = dbstate.LoopEnded
			return e, nil
		}
		if err != nil {
			return e, err
		}
		if fp := fingerprint(t); l.Baseline == nil || *l.Baseline != fp {
			e.baseline = &fp
			if wanted(l.Statuses, t.Status) {
				e.fact = taskFact(t, l.Baseline)
			}
		}
		e.next = now.Add(s.loopPoll())
	case dbstate.LoopTriggerPullRequest:
		t, err := q.GetTaskByID(ctx, deref(l.SubjectTaskID))
		if errors.Is(err, pgx.ErrNoRows) {
			e.state = dbstate.LoopEnded
			return e, nil
		}
		if err != nil {
			return e, err
		}
		if fp := prFingerprint(t); l.Baseline == nil || *l.Baseline != fp {
			e.baseline = &fp
			if len(l.PrConditions) == 0 || prHolds(l.PrConditions, t) {
				e.fact = prFact(t)
			}
		}
		e.next = now.Add(s.loopPoll())
	case dbstate.LoopTriggerTime:
		if l.FireAt != nil && !now.Before(*l.FireAt) {
			e.fact = "timer fired at " + now.UTC().Format(time.RFC3339)
			if l.SubjectTaskID != nil {
				t, err := q.GetTaskByID(ctx, *l.SubjectTaskID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return e, err
				}
				if err == nil {
					e.fact += "; " + taskFact(t, nil)
				}
			}
			if l.EverySeconds == nil {
				e.fireAt = nil
				e.state = dbstate.LoopDone
			} else {
				// The next occurrence after now: a timer that was asleep fires once, not once per
				// interval it missed.
				every := time.Duration(*l.EverySeconds) * time.Second
				at := *l.FireAt
				for !at.After(now) {
					at = at.Add(every)
				}
				e.fireAt = &at
			}
		}
		if e.fireAt != nil && e.fireAt.Before(e.next) {
			e.next = *e.fireAt
		}
	}
	if l.ExpiresAt.Before(e.next) {
		e.next = l.ExpiresAt
	}
	return e, nil
}

// fire records an occurrence: folded into the loop's firing that is still waiting to be sent, or as
// a new firing, which is one more message.
func (s *Service) fire(ctx context.Context, q *dbq.Queries, l dbq.TaskLoop, e *loopEval) error {
	pending, err := q.GetPendingLoopFiring(ctx, l.ID)
	switch {
	case err == nil:
		facts, dropped := appendFact(pending.Facts, e.fact)
		return q.FoldLoopFiring(ctx, dbq.FoldLoopFiringParams{ID: pending.ID, Facts: facts, Folded: pending.Folded + int32(dropped)}) //nolint:gosec // 0 or 1
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if l.Firings >= l.MaxFirings {
		e.state = dbstate.LoopExhausted
		return nil
	}
	now := s.now()
	notBefore := now
	if l.LastFiredAt != nil {
		if at := l.LastFiredAt.Add(time.Duration(l.MinIntervalSeconds) * time.Second); at.After(notBefore) {
			notBefore = at
		}
	}
	f, err := q.InsertLoopFiring(ctx, dbq.InsertLoopFiringParams{
		ID: uuid.NewString(), LoopID: l.ID, Number: l.Firings + 1, Facts: []string{e.fact}, NotBefore: notBefore,
	})
	if err != nil {
		return err
	}
	id := uuid.NewString()
	if err := q.InsertLoopFiringDelivery(ctx, dbq.InsertLoopFiringDeliveryParams{ID: id, LoopFiringID: &f.ID, RecipientKey: l.RecipientKey}); err != nil {
		return err
	}
	if err := s.notify(ctx, q, EventChannel, id); err != nil {
		return err
	}
	e.firings = f.Number
	e.lastAt = &notBefore
	if e.firings >= l.MaxFirings {
		e.state = dbstate.LoopExhausted
	}
	return nil
}

// firingText is what the recipient reads when a loop fires. It reads only what does not change
// once the firing has a message id, so a resend is the same text.
func (s *Service) firingText(ctx context.Context, q *dbq.Queries, firingID string) (string, error) {
	f, err := q.GetLoopFiring(ctx, firingID)
	if err != nil {
		return "", err
	}
	l, err := q.GetTaskLoopByID(ctx, f.LoopID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[loop %s fired: firing %d of at most %d]\nYou asked: %s", l.ID, f.Number, l.MaxFirings, l.Note)
	for _, fact := range f.Facts {
		b.WriteString("\n- " + fact)
	}
	if f.Folded > 0 {
		fmt.Fprintf(&b, "\n(%d more occurrences folded in)", f.Folded)
	}
	switch {
	case f.Number >= l.MaxFirings:
		b.WriteString("\nThis was the loop's last firing (max_firings).")
	case l.Trigger == dbstate.LoopTriggerTime && l.EverySeconds == nil:
		b.WriteString("\nThis one-time timer is done.")
	}
	return b.String(), nil
}
