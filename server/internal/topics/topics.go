// Package topics is the owner's notebook that agents write to through MCP: named topics, each
// with a status line, notes, decisions and pending items.
package topics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

const (
	// Inbox is the topic a record lands in when the caller names none.
	Inbox = "inbox"
	// MaxChars caps a note, a decision, a pending item and a status line.
	MaxChars = 2000
	// MinPrefix is the shortest id prefix that may close a pending item.
	MinPrefix = 8
)

var (
	// ErrPrefixTooShort means the pending id prefix has fewer than [MinPrefix] characters.
	ErrPrefixTooShort = errors.New("topics: give at least 8 characters of the pending id")
	// ErrNoPending means no open pending item of the owner has the id prefix.
	ErrNoPending = errors.New("topics: no such open pending item")
	// ErrAmbiguousPrefix means more than one open pending item has the id prefix, and none was
	// closed.
	ErrAmbiguousPrefix = errors.New("topics: the pending id prefix matches more than one item")
	// ErrBadKind means a record kind other than note, decision or pending.
	ErrBadKind = errors.New("topics: kind must be note, decision or pending")
	// ErrEmpty means the text is blank.
	ErrEmpty = errors.New("topics: text is required")
)

// Service reads and writes one owner's topics.
type Service struct {
	Pool *pgxpool.Pool
}

// NewService builds the topic index over the database.
func NewService(pool *pgxpool.Pool) *Service { return &Service{Pool: pool} }

// Entry is one line of the topic index.
type Entry struct {
	Name       string `json:"name"`
	StatusLine string `json:"status_line"`
	Pending    int    `json:"pending"`
}

// Index lists the owner's topics, most recently updated first, with their open pending counts.
func (s *Service) Index(ctx context.Context, userID string) ([]Entry, error) {
	rows, err := dbq.New(s.Pool).ListTopicsWithPending(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("topics: list: %w", err)
	}
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = Entry{Name: r.Name, StatusLine: r.StatusLine, Pending: int(r.Pending)}
	}
	return out, nil
}

// Open returns the named topic, creating it when needed, and sets its status line when status is
// not nil. A blank name means the inbox.
func (s *Service) Open(ctx context.Context, userID, name string, status *string) (string, error) {
	name = nameOrInbox(name)
	var topic dbq.Topic
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		var err error
		// Opening without a status only reads the topic; it must not move it in the index.
		if topic, err = q.EnsureTopic(ctx, dbq.EnsureTopicParams{ID: uuid.NewString(), UserID: userID, Name: name}); err != nil {
			return err
		}
		if status != nil {
			return q.SetTopicStatusLine(ctx, dbq.SetTopicStatusLineParams{ID: topic.ID, StatusLine: truncate(*status)})
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("topics: open %q: %w", name, err)
	}
	return topic.Name, nil
}

// Recorded is a stored note, decision or pending item.
type Recorded struct {
	ID    string
	Topic string
}

// Add stores a note, decision or pending item in the named topic (the inbox when blank),
// attributed to the session that wrote it.
func (s *Service) Add(ctx context.Context, userID, sessionID string, kind dbstate.TopicRecordKind, text, topic string) (Recorded, error) {
	switch kind {
	case dbstate.RecordNote, dbstate.RecordDecision, dbstate.RecordPending:
	default:
		return Recorded{}, ErrBadKind
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Recorded{}, ErrEmpty
	}
	topic = nameOrInbox(topic)
	rec := Recorded{ID: uuid.NewString(), Topic: topic}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		t, err := q.UpsertTopic(ctx, dbq.UpsertTopicParams{ID: uuid.NewString(), UserID: userID, Name: topic})
		if err != nil {
			return err
		}
		rec.Topic = t.Name
		_, err = q.CreateTopicRecord(ctx, dbq.CreateTopicRecordParams{
			ID: rec.ID, TopicID: t.ID, Kind: kind, Text: truncate(text), SessionID: &sessionID,
		})
		return err
	})
	if err != nil {
		return Recorded{}, fmt.Errorf("topics: record in %q: %w", topic, err)
	}
	return rec, nil
}

// ClosePending closes the owner's open pending item whose id starts with prefix. A prefix that
// matches several items closes none.
func (s *Service) ClosePending(ctx context.Context, userID, prefix string) error {
	if utf8.RuneCountInString(prefix) < MinPrefix {
		return ErrPrefixTooShort
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		ids, err := dbq.New(tx).ClosePendingByPrefix(ctx, dbq.ClosePendingByPrefixParams{UserID: userID, Prefix: prefix})
		if err != nil {
			return err
		}
		switch len(ids) {
		case 1:
			return nil
		case 0:
			return ErrNoPending
		}
		return ErrAmbiguousPrefix // rolls the update back
	})
	if errors.Is(err, ErrNoPending) || errors.Is(err, ErrAmbiguousPrefix) {
		return err
	}
	if err != nil {
		return fmt.Errorf("topics: close pending: %w", err)
	}
	return nil
}

func nameOrInbox(name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return Inbox
}

// truncate cuts s to [MaxChars] characters.
func truncate(s string) string {
	if utf8.RuneCountInString(s) <= MaxChars {
		return s
	}
	return string([]rune(s)[:MaxChars])
}
