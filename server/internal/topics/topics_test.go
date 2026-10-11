package topics_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/testdb"
	"github.com/oldsj/mainloop/server/internal/topics"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func session(t *testing.T, q *dbq.Queries, user string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := q.CreateSession(context.Background(), dbq.CreateSessionParams{
		ID: id, UserID: user, Title: "t", Description: "d", Prompt: "p", BaseBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOpenAndIndex(t *testing.T) {
	d := testdb.New(t)
	svc := topics.NewService(d.Pool)
	ctx := context.Background()

	if idx, err := svc.Index(ctx, "u1"); err != nil || len(idx) != 0 {
		t.Fatalf("empty Index = %v, %v", idx, err)
	}
	status := "  shipping the migration "
	name, err := svc.Open(ctx, "u1", "migration", &status)
	if err != nil || name != "migration" {
		t.Fatalf("Open = %q, %v", name, err)
	}
	// Opening again without a status keeps the line; with one, replaces it.
	if _, err := svc.Open(ctx, "u1", "migration", nil); err != nil {
		t.Fatal(err)
	}
	idx, _ := svc.Index(ctx, "u1")
	if len(idx) != 1 || idx[0].Name != "migration" || strings.TrimSpace(idx[0].StatusLine) != "shipping the migration" {
		t.Errorf("Index = %+v", idx)
	}
	next := "done"
	if _, err := svc.Open(ctx, "u1", "migration", &next); err != nil {
		t.Fatal(err)
	}
	if idx, _ = svc.Index(ctx, "u1"); idx[0].StatusLine != "done" {
		t.Errorf("status line = %q", idx[0].StatusLine)
	}
	// Topics are per owner.
	if other, _ := svc.Index(ctx, "u2"); len(other) != 0 {
		t.Errorf("u2 sees %+v", other)
	}
}

func TestOpenWithoutAStatusDoesNotBumpTheTopic(t *testing.T) {
	d := testdb.New(t)
	svc := topics.NewService(d.Pool)
	ctx := context.Background()
	if _, err := svc.Open(ctx, "u1", "older", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open(ctx, "u1", "newer", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE topics SET updated_at = now() - interval '1 hour' WHERE name = 'older'`); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT updated_at FROM topics WHERE name = 'older'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open(ctx, "u1", "older", nil); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT updated_at FROM topics WHERE name = 'older'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Errorf("updated_at moved from %v to %v", before, after)
	}
	if idx, _ := svc.Index(ctx, "u1"); len(idx) != 2 || idx[0].Name != "newer" {
		t.Errorf("Index = %+v", idx)
	}
}

func TestAddDefaultsToInboxAndCountsPending(t *testing.T) {
	d := testdb.New(t)
	q := dbq.New(d.Pool)
	svc := topics.NewService(d.Pool)
	ctx := context.Background()
	sid := session(t, q, "u1")

	note, err := svc.Add(ctx, "u1", sid, dbstate.RecordNote, "remember this", "")
	if err != nil || note.Topic != topics.Inbox {
		t.Fatalf("Add = %+v, %v", note, err)
	}
	if _, err := svc.Add(ctx, "u1", sid, dbstate.RecordPending, "ship it", "release"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, "u1", sid, dbstate.RecordPending, "and this", "release"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, "u1", sid, dbstate.RecordDecision, "use Go", "release"); err != nil {
		t.Fatal(err)
	}
	idx, _ := svc.Index(ctx, "u1")
	counts := map[string]int{}
	for _, e := range idx {
		counts[e.Name] = int(e.Pending)
	}
	if len(counts) != 2 || counts["release"] != 2 || counts[topics.Inbox] != 0 {
		t.Errorf("counts = %v", counts)
	}
	for _, kind := range []dbstate.TopicRecordKind{dbstate.RecordReport, "bogus"} {
		if _, err := svc.Add(ctx, "u1", sid, kind, "x", ""); !errors.Is(err, topics.ErrBadKind) {
			t.Errorf("Add(%s) err = %v, want ErrBadKind", kind, err)
		}
	}
	if _, err := svc.Add(ctx, "u1", sid, dbstate.RecordNote, "   ", ""); !errors.Is(err, topics.ErrEmpty) {
		t.Errorf("blank text err = %v", err)
	}
	long, err := svc.Add(ctx, "u1", sid, dbstate.RecordNote, strings.Repeat("é", topics.MaxChars+50), "")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := q.ListTopicRecords(ctx, mustTopicID(t, q, "u1", topics.Inbox))
	for _, r := range rec {
		if r.ID == long.ID && len([]rune(r.Text)) != topics.MaxChars {
			t.Errorf("stored %d characters", len([]rune(r.Text)))
		}
	}
}

func mustTopicID(t *testing.T, q *dbq.Queries, user, name string) string {
	t.Helper()
	topic, err := q.GetTopicByName(context.Background(), dbq.GetTopicByNameParams{UserID: user, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return topic.ID
}

func TestClosePending(t *testing.T) {
	d := testdb.New(t)
	q := dbq.New(d.Pool)
	svc := topics.NewService(d.Pool)
	ctx := context.Background()
	sid := session(t, q, "u1")
	p, _ := svc.Add(ctx, "u1", sid, dbstate.RecordPending, "one", "")
	n, _ := svc.Add(ctx, "u1", sid, dbstate.RecordNote, "not pending", "")

	if err := svc.ClosePending(ctx, "u1", p.ID[:4]); !errors.Is(err, topics.ErrPrefixTooShort) {
		t.Errorf("short prefix err = %v", err)
	}
	if err := svc.ClosePending(ctx, "u2", p.ID); !errors.Is(err, topics.ErrNoPending) {
		t.Errorf("another owner's item err = %v", err)
	}
	if err := svc.ClosePending(ctx, "u1", n.ID); !errors.Is(err, topics.ErrNoPending) {
		t.Errorf("a note err = %v", err)
	}
	if err := svc.ClosePending(ctx, "u1", p.ID[:8]); err != nil {
		t.Fatalf("close by prefix: %v", err)
	}
	if err := svc.ClosePending(ctx, "u1", p.ID); !errors.Is(err, topics.ErrNoPending) {
		t.Errorf("closing twice err = %v", err)
	}
	if idx, _ := svc.Index(ctx, "u1"); idx[0].Pending != 0 {
		t.Errorf("pending = %d", idx[0].Pending)
	}
}

func TestClosePendingRefusesAnAmbiguousPrefix(t *testing.T) {
	d := testdb.New(t)
	q := dbq.New(d.Pool)
	svc := topics.NewService(d.Pool)
	ctx := context.Background()
	sid := session(t, q, "u1")
	topic, err := q.CreateTopic(ctx, dbq.CreateTopicParams{ID: uuid.NewString(), UserID: "u1", Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aaaaaaaa-1", "aaaaaaaa-2"} {
		if _, err := q.CreateTopicRecord(ctx, dbq.CreateTopicRecordParams{
			ID: id, TopicID: topic.ID, Kind: dbstate.RecordPending, Text: "x", SessionID: &sid,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.ClosePending(ctx, "u1", "aaaaaaaa"); !errors.Is(err, topics.ErrAmbiguousPrefix) {
		t.Fatalf("err = %v, want ErrAmbiguousPrefix", err)
	}
	// Nothing was closed.
	if idx, _ := svc.Index(ctx, "u1"); idx[0].Pending != 2 {
		t.Errorf("pending = %d after a refused close", idx[0].Pending)
	}
	if err := svc.ClosePending(ctx, "u1", "aaaaaaaa-1"); err != nil {
		t.Errorf("exact id: %v", err)
	}
}
