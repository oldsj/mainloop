package transcripts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Kagent is what ingestion reads from kagent. *kagent.Client implements it through [FromClient].
type Kagent interface {
	// ListTasksPage reads one page of a conversation's tasks, oldest first, without artifacts.
	ListTasksPage(ctx context.Context, agent kagent.AgentRef, contextID, token string, limit int) ([]*a2a.Task, string, error)
	// GetTask reads a task with its whole history and artifacts.
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
	// AgentExists reports whether kagent has the Agent; only kagent's own not-found is false.
	AgentExists(ctx context.Context, agent kagent.AgentRef) (bool, error)
}

// AgentResolver names the Agent that runs a binding, by the rule the sessions package talks to it.
type AgentResolver interface {
	ResolveAgent(agentNamespace, agentName, kind *string, role dbstate.BindingRole) (kagent.AgentRef, error)
}

// Service copies finished turns from kagent into transcript entries.
type Service struct {
	Pool    *pgxpool.Pool
	Kagent  Kagent
	Agents  AgentResolver
	OwnerID string
	Log     *slog.Logger
	Options Options
	// now is the clock; nil is time.Now.
	now func() time.Time
}

// Options tunes ingestion. The zero value of each field takes its default.
type Options struct {
	// Every is the sweep period. A finished turn nudges its session due, so this is about how soon
	// after a turn ends it is copied.
	Every time.Duration
	// Rest is how long a session that was read without failing waits before it is read again
	// without a nudge (a turn Mainloop did not send, or one whose nudge was missed).
	Rest time.Duration
	// MaxSessions bounds the sessions of one pass. A session's scan reads at most MaxPages pages of
	// PageSize tasks and copies at most CopiesPerScan tasks; one that reaches either is due again at
	// once.
	MaxSessions, MaxPages, PageSize, CopiesPerScan int
	// ReadTimeout bounds one kagent read.
	ReadTimeout time.Duration
	// Backoff and BackoffCeiling pace a session whose reads fail; MaxFailures in a row make it
	// unavailable.
	Backoff, BackoffCeiling time.Duration
	MaxFailures             int
	// Limits bound each entry; Budget bounds the entry text of one kagent Session, in bytes.
	Limits Limits
	Budget int64
	// Retention is how long a copied turn's entries are kept; RetainEvery is the retention period
	// and RetainBatch the tasks pruned per statement.
	Retention   time.Duration
	RetainEvery time.Duration
	RetainBatch int
}

func (s *Service) opts() Options {
	o := s.Options
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	defInt := func(n *int, v int) {
		if *n <= 0 {
			*n = v
		}
	}
	def(&o.Every, 10*time.Second)
	def(&o.Rest, 15*time.Minute)
	defInt(&o.MaxSessions, 20)
	defInt(&o.MaxPages, 200)
	defInt(&o.CopiesPerScan, 50)
	defInt(&o.PageSize, 50)
	def(&o.ReadTimeout, 30*time.Second)
	def(&o.Backoff, 30*time.Second)
	def(&o.BackoffCeiling, time.Hour)
	defInt(&o.MaxFailures, 24)
	if o.Limits.Text <= 0 || o.Limits.Result <= 0 {
		o.Limits = DefaultLimits
	}
	if o.Budget <= 0 {
		o.Budget = 20 << 20
	}
	def(&o.Retention, 90*24*time.Hour)
	def(&o.RetainEvery, 10*time.Minute)
	defInt(&o.RetainBatch, 500)
	return o
}

// NewService builds the ingester from the configuration.
func NewService(cfg *config.Config, pool *pgxpool.Pool, k Kagent, agents AgentResolver, log *slog.Logger) *Service {
	return &Service{
		Pool: pool, Kagent: k, Agents: agents, OwnerID: cfg.Identity.OwnerID, Log: log,
		Options: Options{
			Retention: time.Duration(cfg.Transcripts.RetentionDays) * 24 * time.Hour,
			Budget:    int64(cfg.Transcripts.SessionBudgetMiB) << 20,
		},
	}
}

// Register adds the ingest and retention sweeps.
func (s *Service) Register(r *reconcile.Runner) {
	o := s.opts()
	r.AddSweep(reconcile.Sweep{Name: "transcript-ingest", Every: o.Every, Run: s.Ingest})
	r.AddSweep(reconcile.Sweep{Name: "transcript-retention", Every: o.RetainEvery, Run: func(ctx context.Context) error {
		_, err := s.Retain(ctx)
		return err
	}})
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Ingest makes one pass: it starts following newly bound kagent Sessions, then reads each due one
// from its cursor and copies the turns that have finished. kagent is read with no transaction open;
// each task is written in its own short transaction, and a cursor moves only past a page whose
// tasks are all copied, so a pass that dies half way repeats work and loses none.
func (s *Service) Ingest(ctx context.Context) error {
	if err := s.adopt(ctx); err != nil {
		return fmt.Errorf("transcripts: adopt sessions: %w", err)
	}
	o := s.opts()
	due, err := dbq.New(s.Pool).ListDueTranscriptSessions(ctx, dbq.ListDueTranscriptSessionsParams{
		OwnerID: s.OwnerID, MaxSessions: int32(o.MaxSessions), //nolint:gosec // small bounded option
	})
	if err != nil {
		return fmt.Errorf("transcripts: list due sessions: %w", err)
	}
	for _, ts := range due {
		_, err := s.scan(ctx, ts)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stopsPass(err) {
			// kagent is down or refuses our credentials: every session would fail the same way, and
			// none of them is to blame.
			return fmt.Errorf("transcripts: %w", err)
		}
		s.fail(ctx, ts, err)
	}
	return nil
}

// Flush copies every turn a kagent Session has finished, now, outside the sweep and past the sweep's
// per-pass bounds. It is for a caller about to delete the Session, which makes kagent's record of it
// unreadable. It stops when ctx does. A Session no binding names is not followed, and Flush does
// nothing for it.
func (s *Service) Flush(ctx context.Context, kagentSessionID string) error {
	q := dbq.New(s.Pool)
	ts, err := q.GetTranscriptSession(ctx, kagentSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.adopt(ctx); err != nil {
			return err
		}
		ts, err = q.GetTranscriptSession(ctx, kagentSessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
	}
	for err == nil && ts.State == string(StateActive) {
		var more bool
		if more, err = s.scan(ctx, ts); err != nil || !more {
			break
		}
		if err = ctx.Err(); err == nil {
			ts, err = q.GetTranscriptSession(ctx, kagentSessionID)
		}
	}
	return err
}

// adopt follows the owner's bound kagent Sessions it does not follow yet. A binding whose Agent cannot
// be resolved is skipped and tried again on the next pass.
func (s *Service) adopt(ctx context.Context) error {
	q := dbq.New(s.Pool)
	rows, err := q.ListTranscriptBindingsToAdopt(ctx, dbq.ListTranscriptBindingsToAdoptParams{UserID: s.OwnerID, MaxRows: 100})
	if err != nil {
		return err
	}
	for _, b := range rows {
		agent, err := s.Agents.ResolveAgent(&b.ResolvedAgentNamespace, &b.ResolvedAgentName, &b.Kind, b.Role)
		if err != nil {
			s.log().WarnContext(ctx, "transcripts: cannot tell which agent runs a session",
				"session_id", b.SessionID, "kagent_session_id", *b.KagentSessionID, "error", err)
			continue
		}
		if err := q.InsertTranscriptSession(ctx, dbq.InsertTranscriptSessionParams{
			KagentSessionID: *b.KagentSessionID, SessionID: b.SessionID, OwnerID: b.UserID,
			AgentNamespace: agent.Namespace, AgentName: agent.Name,
		}); err != nil {
			return err
		}
	}
	return nil
}

// scan reads one session's tasks from its cursor and copies every finished task that has no copy
// yet, up to CopiesPerScan copies. A task still running, or waiting for input, is passed over (a
// waiting task continues the same turn when it is answered) and holds the cursor on its page, so
// every pass looks at it again; the pages after it are still read and their finished tasks copied.
// Pages whose tasks were all copied before cost a listing and no task read.
//
// more reports that the scan stopped at a bound with work possibly left.
func (s *Service) scan(ctx context.Context, ts dbq.TranscriptSession) (more bool, err error) {
	o := s.opts()
	q := dbq.New(s.Pool)
	agent := kagent.AgentRef{Namespace: ts.AgentNamespace, Name: ts.AgentName}
	token := ""
	if ts.TaskCursor != nil {
		token = *ts.TaskCursor
	}
	stored, err := q.TranscriptSessionBytes(ctx, ts.KagentSessionID)
	if err != nil {
		return false, err
	}
	cursor, held, rest := ts.TaskCursor, false, o.Rest
	copies := o.CopiesPerScan
	for page := 1; ; page++ {
		rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
		tasks, next, err := s.Kagent.ListTasksPage(rctx, agent, ts.ContextID, token, o.PageSize)
		cancel()
		if err != nil {
			var perr *kagent.ProtocolError
			if token != "" && errors.As(err, &perr) {
				// kagent no longer takes the cursor. Start over: copied tasks are skipped by receipt.
				s.log().WarnContext(ctx, "transcripts: task cursor rejected; reading from the start",
					"kagent_session_id", ts.KagentSessionID)
				return true, q.SetTranscriptSessionScan(ctx, dbq.SetTranscriptSessionScanParams{
					KagentSessionID: ts.KagentSessionID, RestSeconds: 0,
				})
			}
			if s.retireGone(ctx, ts, agent, err) {
				return false, nil
			}
			return false, err
		}
		pending, err := s.copyPage(ctx, ts, agent, tasks, &stored, &copies)
		if err != nil {
			if s.retireGone(ctx, ts, agent, err) {
				return false, nil
			}
			return false, err
		}
		held = held || pending
		// The cursor passes a page only once nothing on it or before it is left to copy and a page
		// follows. The last page keeps the cursor: new tasks are added to it.
		if !held && next != "" {
			cursor = &next
		}
		if copies <= 0 {
			rest, more = 0, true // more may be waiting: due again at once
			break
		}
		if next == "" || ctx.Err() != nil {
			break
		}
		if page >= o.MaxPages {
			// Pages are left. With the cursor moved, the next scan starts past what this one read:
			// due again at once. A held cursor would make it read the same pages again, so the pages
			// past the bound wait until the held task finishes.
			if !held {
				rest, more = 0, true
			} else {
				s.log().WarnContext(ctx, "transcripts: a waiting task holds the cursor; later pages wait for it",
					"kagent_session_id", ts.KagentSessionID, "pages", o.MaxPages)
			}
			break
		}
		token = next
	}
	return more, q.SetTranscriptSessionScan(ctx, dbq.SetTranscriptSessionScanParams{
		KagentSessionID: ts.KagentSessionID, Cursor: cursor, RestSeconds: rest.Seconds(),
	})
}

// copyPage copies the finished tasks of one page that have no copy yet, while copies lasts, and
// reports whether any task on the page is left uncopied (unfinished, or past the copies).
func (s *Service) copyPage(
	ctx context.Context, ts dbq.TranscriptSession, agent kagent.AgentRef, tasks []*a2a.Task, stored *int64, copies *int,
) (pending bool, err error) {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t != nil {
			ids = append(ids, string(t.ID))
		}
	}
	copied, err := dbq.New(s.Pool).CopiedTranscriptTasks(ctx, ids)
	if err != nil {
		return false, err
	}
	done := make(map[string]bool, len(copied))
	for _, id := range copied {
		done[id] = true
	}
	for _, t := range tasks {
		if t == nil || done[string(t.ID)] {
			continue
		}
		if !Finished(t.Status.State) || *copies <= 0 {
			pending = true
			continue
		}
		*copies--
		ok, err := s.copyTask(ctx, ts, agent, string(t.ID), stored)
		if err != nil {
			return pending, err
		}
		pending = pending || !ok
	}
	return pending, nil
}

// copyTask reads one task whole and writes it in a short transaction of its own. It reports false
// when the task turned out not to be finished after all.
func (s *Service) copyTask(ctx context.Context, ts dbq.TranscriptSession, agent kagent.AgentRef, taskID string, stored *int64) (bool, error) {
	o := s.opts()
	rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
	task, err := s.Kagent.GetTask(rctx, agent, taskID)
	cancel()
	if err != nil {
		return false, err
	}
	if !Finished(task.Status.State) {
		return false, nil // listed as finished but read otherwise: a later pass decides
	}
	entries := Budget(Normalize(task, o.Limits), *stored, o.Budget)
	c := Copy{
		KagentSessionID: ts.KagentSessionID, SessionID: ts.SessionID, TaskID: taskID,
		TaskState: string(task.Status.State), TaskAt: task.Status.Timestamp, Entries: entries,
	}
	var wrote bool
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		wrote, err = Save(ctx, dbq.New(tx), c)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("save task %s: %w", taskID, err)
	}
	if wrote {
		for _, e := range entries {
			*stored += int64(len(e.Text))
		}
	}
	return true, nil
}

// Budget stubs every entry past the session's text budget: kind, tool and sizes are kept, the text
// is not. stored is what the session already holds.
func Budget(entries []Entry, stored, budget int64) []Entry {
	for i := range entries {
		e := &entries[i]
		if stored+int64(len(e.Text)) > budget {
			e.Text, e.Truncated = "", true
			continue
		}
		stored += int64(len(e.Text))
	}
	return entries
}

// Finished reports whether a task's turn is over for good. A task that waits for input is not: it
// continues the same turn when it is answered.
func Finished(st a2a.TaskState) bool {
	switch st {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	}
	return false
}

// retireGone ends a session whose conversation kagent no longer has, and reports whether it did. kagent
// answers a missing conversation and a missing Agent with the same not-found, so the session is given
// up only once kagent shows the Agent. It is complete when every turn Mainloop sent into it was copied,
// and unavailable otherwise.
func (s *Service) retireGone(ctx context.Context, ts dbq.TranscriptSession, agent kagent.AgentRef, err error) bool {
	var gone *kagent.TaskNotFound
	if !errors.As(err, &gone) {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, s.opts().ReadTimeout)
	exists, aerr := s.Kagent.AgentExists(rctx, agent)
	cancel()
	if aerr != nil || !exists {
		return false
	}
	q := dbq.New(s.Pool)
	missing, cerr := q.CountUncopiedDeliveries(ctx, &ts.KagentSessionID)
	if cerr != nil {
		return false
	}
	state, reason := StateComplete, ""
	if missing > 0 {
		state, reason = StateUnavailable, fmt.Sprintf("kagent no longer has this session; %d turns were not copied", missing)
	}
	if eerr := q.EndTranscriptSession(ctx, dbq.EndTranscriptSessionParams{
		KagentSessionID: ts.KagentSessionID, State: string(state), Reason: reason,
	}); eerr != nil {
		return false
	}
	s.log().InfoContext(ctx, "transcripts: kagent session gone; no longer followed",
		"kagent_session_id", ts.KagentSessionID, "session_id", ts.SessionID, "state", state, "uncopied", missing)
	return true
}

// fail backs a session off after a failed read, longer after each failure in a row, and gives it up
// as unavailable after MaxFailures.
func (s *Service) fail(ctx context.Context, ts dbq.TranscriptSession, err error) {
	o := s.opts()
	q := dbq.New(s.Pool)
	s.log().WarnContext(ctx, "transcripts: scan failed", "kagent_session_id", ts.KagentSessionID, "failures", ts.Failures+1, "error", err)
	if int(ts.Failures)+1 >= o.MaxFailures {
		if eerr := q.EndTranscriptSession(ctx, dbq.EndTranscriptSessionParams{
			KagentSessionID: ts.KagentSessionID, State: string(StateUnavailable),
			Reason: fmt.Sprintf("gave up after %d failed reads", o.MaxFailures),
		}); eerr != nil {
			s.log().WarnContext(ctx, "transcripts: give up session", "kagent_session_id", ts.KagentSessionID, "error", eerr)
		}
		return
	}
	delay := min(o.Backoff<<min(ts.Failures, 16), o.BackoffCeiling)
	if ferr := q.FailTranscriptSessionScan(ctx, dbq.FailTranscriptSessionScanParams{
		KagentSessionID: ts.KagentSessionID, DelaySeconds: delay.Seconds(),
	}); ferr != nil {
		s.log().WarnContext(ctx, "transcripts: back off session", "kagent_session_id", ts.KagentSessionID, "error", ferr)
	}
}

// stopsPass reports a failure every session would meet: kagent is unreachable, or it refused our
// own credentials.
func stopsPass(err error) bool {
	var (
		down *kagent.Unreachable
		cfg  *kagent.ConfigError
	)
	return errors.As(err, &down) || (errors.As(err, &cfg) && cfg.Credentials)
}

// Retain deletes the entries of turns copied longer ago than the retention, in batches, and reports
// how many tasks it pruned. Their receipts stay, so kagent's copy is not taken again.
func (s *Service) Retain(ctx context.Context) (int64, error) {
	o := s.opts()
	cutoff := s.clock().Add(-o.Retention)
	var total int64
	for {
		n, err := dbq.New(s.Pool).PruneTranscriptTasks(ctx, dbq.PruneTranscriptTasksParams{
			Cutoff: cutoff, MaxRows: int32(o.RetainBatch), //nolint:gosec // small bounded option
		})
		if err != nil {
			return total, fmt.Errorf("transcripts: prune: %w", err)
		}
		total += n
		if n < int64(o.RetainBatch) || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}
