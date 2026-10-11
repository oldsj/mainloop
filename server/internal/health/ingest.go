package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// The gaps a receipt can carry: what the page could not see.
const (
	GapOverflow          = "overflow"
	GapInvalidEvent      = "invalid_event"
	GapIdentityMismatch  = "identity_mismatch"
	GapIdentityConflict  = "identity_conflict"
	GapInvalidTimestamp  = "invalid_timestamp"
	GapSourceUnavailable = "source_unavailable"
	GapProducerRestart   = "producer_restart"
)

const (
	// maxEventBytes bounds one observation, as compact JSON.
	maxEventBytes = 8192
	// maxReceiptEvents bounds the observations one kagent task may store.
	maxReceiptEvents = 2048
	// futureSkew is how far ahead of now an observation's timestamp may be.
	futureSkew = 5 * time.Minute
)

// ErrSessionGone is returned by Ingest when the session the task belongs to no longer exists.
var ErrSessionGone = errors.New("session no longer exists")

// Input is one kagent task that ended, ready to store.
type Input struct {
	OwnerID    string
	Gateway    string
	SessionID  string
	DeliveryID string
	// ReceiptAt is when the delivery was created; the page's coverage is counted on it.
	ReceiptAt time.Time
	// TaskID and ContextID are the task's id and context id as kagent reports them.
	TaskID    string
	ContextID string
	// RuntimeSessionID is the kagent session the delivery was sent on. ContextID must equal it.
	RuntimeSessionID string
	Artifacts        []*a2a.Artifact
	// Now and EventRetention fix the expiry rule: an observation at or before Now-EventRetention is
	// not stored.
	Now            time.Time
	EventRetention time.Duration
}

// Result says what an Ingest stored.
type Result struct {
	// Stored is the number of new observations written.
	Stored int
	// Turns is the number of turn rollups rebuilt.
	Turns int
	// Reasons are the gaps found in this task, sorted.
	Reasons []string
}

// Ingest stores the observations of one kagent task inside the caller's transaction and rebuilds
// the rollups of the turns they changed. It is idempotent: the same task again stores nothing and
// leaves every rollup as it was. It does no network I/O, so the transaction is only held for the
// writes.
//
// Concurrency: a per-receipt transaction lock serialises two ingests of one task, and the turns
// an ingest changes are locked FOR UPDATE in key order, so a retention pass cannot freeze a turn
// halfway through its rebuild. A frozen turn takes no more observations.
//
// The caller marks nothing else: Ingest writes the receipt (collected, with its gaps) itself, so
// the observations and the receipt that says they were read commit together.
func Ingest(ctx context.Context, tx pgx.Tx, in Input) (Result, error) {
	q := dbq.New(tx)
	attr, err := q.GetHealthAttribution(ctx, dbq.GetHealthAttributionParams{ID: in.SessionID, UserID: in.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrSessionGone
	}
	if err != nil {
		return Result{}, fmt.Errorf("health: attribution: %w", err)
	}
	projectID := attr.SessionProjectID
	if attr.AttemptID != nil {
		projectID = attr.TaskProjectID
	}
	if err := db.XactLock(ctx, tx, fmt.Sprintf("health:%s:%s:%s:%s", in.OwnerID, in.Gateway, in.SessionID, in.TaskID)); err != nil {
		return Result{}, fmt.Errorf("health: lock: %w", err)
	}

	reasons := map[string]bool{}
	cutoff := in.Now.Add(-in.EventRetention)

	type candidate struct {
		ev    Event
		canon []byte
	}
	var cands []candidate
	for _, art := range in.Artifacts {
		if art == nil {
			continue
		}
		for _, part := range art.Parts {
			if part == nil {
				continue
			}
			raw := part.Data()
			if !IsHealthPart(art.Name, raw) {
				continue
			}
			encoded, err := json.Marshal(raw)
			if err != nil || len(encoded) > maxEventBytes {
				reasons[GapOverflow] = true
				continue
			}
			ev, err := ParseEvent(raw)
			if err != nil {
				reasons[GapInvalidEvent] = true
				continue
			}
			if ev.A2ATaskID != in.TaskID || ev.RuntimeSessionID != in.ContextID || in.ContextID != in.RuntimeSessionID ||
				attr.Provider != "claude" || string(art.ID) != ev.EventID {
				reasons[GapIdentityMismatch] = true
				continue
			}
			if !ev.OccurredAt.After(cutoff) {
				continue // an expired replay must not restore detail
			}
			payload, err := ev.Payload()
			if err != nil {
				return Result{}, err
			}
			canon, err := Canonical(payload)
			if err != nil {
				return Result{}, err
			}
			cands = append(cands, candidate{ev: ev, canon: canon})
		}
	}

	// Lock the turns these observations belong to before reading what is stored, so a retention
	// pass and this ingest never interleave on a turn.
	turnKeys := map[string]bool{}
	for _, c := range cands {
		turnKeys[c.ev.TurnKey] = true
	}
	frozen := map[string]bool{}
	if len(turnKeys) > 0 {
		keys := sortedKeys(turnKeys)
		locked, err := q.LockHealthTurns(ctx, dbq.LockHealthTurnsParams{
			OwnerID: in.OwnerID, Gateway: in.Gateway, SessionID: in.SessionID, A2aTaskID: in.TaskID, TurnKeys: keys,
		})
		if err != nil {
			return Result{}, fmt.Errorf("health: lock turns: %w", err)
		}
		for _, t := range locked {
			frozen[t.TurnKey] = t.Frozen
		}
	}

	type stored struct {
		canon     []byte
		sessionID string
		taskID    string
	}
	existing := map[string]stored{}
	if len(cands) > 0 {
		ids := make([]string, len(cands))
		for i, c := range cands {
			ids[i] = c.ev.EventID
		}
		rows, err := q.GetHealthEvents(ctx, dbq.GetHealthEventsParams{OwnerID: in.OwnerID, Gateway: in.Gateway, EventIds: ids})
		if err != nil {
			return Result{}, fmt.Errorf("health: read events: %w", err)
		}
		for _, r := range rows {
			canon, err := Canonical(r.Payload)
			if err != nil {
				return Result{}, err
			}
			existing[r.EventID] = stored{canon: canon, sessionID: r.SessionID, taskID: r.A2aTaskID}
		}
	}
	count := int64(0)
	if len(cands) > 0 {
		count, err = q.CountHealthReceiptEvents(ctx, dbq.CountHealthReceiptEventsParams{
			OwnerID: in.OwnerID, Gateway: in.Gateway, SessionID: in.SessionID, A2aTaskID: in.TaskID,
		})
		if err != nil {
			return Result{}, fmt.Errorf("health: count events: %w", err)
		}
	}

	changed := map[string]bool{}
	res := Result{}
	for _, c := range cands {
		ev := c.ev
		if frozen[ev.TurnKey] {
			continue
		}
		if ev.OccurredAt.After(in.Now.Add(futureSkew)) {
			reasons[GapInvalidTimestamp] = true
			continue
		}
		if prev, ok := existing[ev.EventID]; ok {
			if !bytes.Equal(prev.canon, c.canon) || prev.sessionID != in.SessionID || prev.taskID != in.TaskID {
				reasons[GapIdentityConflict] = true
			}
			continue
		}
		if count >= maxReceiptEvents {
			reasons[GapOverflow] = true
			continue
		}
		payload, _ := ev.Payload()
		n, err := q.InsertHealthEvent(ctx, dbq.InsertHealthEventParams{
			OwnerID: in.OwnerID, Gateway: in.Gateway, EventID: ev.EventID, ProducerEpoch: ev.ProducerEpoch,
			Sequence: ev.Sequence, SessionID: in.SessionID, RuntimeSessionID: in.ContextID, A2aTaskID: in.TaskID,
			ProjectID: projectID, TaskID: attr.TaskID, AttemptID: attr.AttemptID, ProfileRevision: attr.ProfileRevision,
			TurnKey: ev.TurnKey, OccurredAt: ev.OccurredAt, Payload: payload,
		})
		if err != nil {
			return Result{}, fmt.Errorf("health: store event: %w", err)
		}
		if n == 0 {
			// The (epoch, sequence) pair is taken by an event with another id.
			reasons[GapIdentityConflict] = true
			continue
		}
		existing[ev.EventID] = stored{canon: c.canon, sessionID: in.SessionID, taskID: in.TaskID}
		count++
		res.Stored++
		changed[ev.TurnKey] = true
	}

	for _, key := range sortedKeys(changed) {
		if err := rebuildTurn(ctx, q, in, attr, projectID, key); err != nil {
			return Result{}, err
		}
		res.Turns++
	}

	res.Reasons = sortedKeys(reasons)
	if err := q.UpsertHealthReceipt(ctx, dbq.UpsertHealthReceiptParams{
		OwnerID: in.OwnerID, Gateway: in.Gateway, SessionID: in.SessionID, A2aTaskID: in.TaskID,
		DeliveryID: in.DeliveryID, ReceiptAt: in.ReceiptAt, ProjectID: projectID, Reasons: res.Reasons,
	}); err != nil {
		return Result{}, fmt.Errorf("health: write receipt: %w", err)
	}
	return res, nil
}

// rebuildTurn recomputes one turn's rollup from all of its stored observations.
func rebuildTurn(ctx context.Context, q *dbq.Queries, in Input, attr dbq.GetHealthAttributionRow, projectID *string, turnKey string) error {
	rows, err := q.ListHealthTurnEvents(ctx, dbq.ListHealthTurnEventsParams{
		OwnerID: in.OwnerID, Gateway: in.Gateway, SessionID: in.SessionID, A2aTaskID: in.TaskID, TurnKey: turnKey,
	})
	if err != nil {
		return fmt.Errorf("health: read turn: %w", err)
	}
	events := make([]Event, 0, len(rows))
	var last time.Time
	for _, r := range rows {
		var raw any
		if err := json.Unmarshal(r.Payload, &raw); err != nil {
			return fmt.Errorf("health: stored event: %w", err)
		}
		ev, err := ParseEvent(raw)
		if err != nil {
			return fmt.Errorf("health: stored event: %w", err)
		}
		events = append(events, ev)
		if r.OccurredAt.After(last) {
			last = r.OccurredAt
		}
	}
	summary, err := json.Marshal(Summarize(events))
	if err != nil {
		return err
	}
	if err := q.UpsertHealthTurn(ctx, dbq.UpsertHealthTurnParams{
		OwnerID: in.OwnerID, Gateway: in.Gateway, SessionID: in.SessionID, A2aTaskID: in.TaskID, TurnKey: turnKey,
		RuntimeSessionID: in.ContextID, ProjectID: projectID, TaskID: attr.TaskID, AttemptID: attr.AttemptID,
		ProfileRevision: attr.ProfileRevision, Provider: attr.Provider, OccurredAt: last, Summary: summary,
		AggregationVersion: AggregationVersion,
	}); err != nil {
		return fmt.Errorf("health: write turn: %w", err)
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
