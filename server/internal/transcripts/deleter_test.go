package transcripts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

type recordingDeleter struct{ calls *[]string }

func (d recordingDeleter) DeleteSession(_ context.Context, id string) (*kagentapi.Session, error) {
	*d.calls = append(*d.calls, "delete "+id)
	return &kagentapi.Session{Id: id}, nil
}

func (d recordingDeleter) ForceDeleteSession(_ context.Context, id string) (*kagentapi.Session, error) {
	*d.calls = append(*d.calls, "force "+id)
	return &kagentapi.Session{Id: id}, nil
}

func TestFlushingKagentCopiesBeforeEveryDelete(t *testing.T) {
	var calls []string
	k := &FlushingKagent{
		flush: func(_ context.Context, id string) error {
			calls = append(calls, "flush "+id)
			return errors.New("kagent hiccup")
		},
		deleter: recordingDeleter{&calls}, timeout: time.Second, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := k.DeleteSession(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ForceDeleteSession(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	want := []string{"flush a", "delete a", "flush b", "force b"}
	if len(calls) != len(want) {
		t.Fatalf("calls %v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls %v, want %v", calls, want)
		}
	}
}

func TestFlushingKagentBoundsTheCopy(t *testing.T) {
	var calls []string
	k := &FlushingKagent{
		flush: func(ctx context.Context, _ string) error {
			<-ctx.Done() // a copy that would never end on its own
			return ctx.Err()
		},
		deleter: recordingDeleter{&calls}, timeout: 50 * time.Millisecond, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	start := time.Now()
	if _, err := k.DeleteSession(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || len(calls) != 1 {
		t.Fatalf("delete after %v, calls %v", time.Since(start), calls)
	}
}

func TestRetireDeletesOnlyAfterACopy(t *testing.T) {
	var calls []string
	failing := true
	k := &FlushingKagent{
		flush: func(_ context.Context, id string) error {
			calls = append(calls, "flush "+id)
			if failing {
				return errors.New("kagent hiccup")
			}
			return nil
		},
		deleter: recordingDeleter{&calls}, timeout: time.Second, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// When the copy fails, Then nothing is deleted
	if _, err := k.RetireSession(context.Background(), "a"); err == nil {
		t.Fatal("a retire whose copy failed reported success")
	}
	// When it succeeds, Then the Session is force-deleted after it
	failing = false
	if _, err := k.RetireSession(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	want := []string{"flush a", "flush b", "force b"}
	if len(calls) != len(want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls %v, want %v", calls, want)
		}
	}
}
