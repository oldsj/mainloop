package events_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func publish(t *testing.T, d *testdb.DB, user string, typ events.Type, payload any) int64 {
	t.Helper()
	var id int64
	err := db.InTx(context.Background(), d.Pool, func(tx pgx.Tx) (err error) {
		id, err = events.Publish(context.Background(), tx, user, typ, payload)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func count(t *testing.T, d *testdb.DB) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPublishIsAtomicWithTheCallersTransaction(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	rollback := errors.New("domain change failed")
	err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := events.Publish(ctx, tx, "u", events.TypeInboxUpdated, map[string]string{}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("err = %v", err)
	}
	if n := count(t, d); n != 0 {
		t.Errorf("rolled-back publish left %d events", n)
	}
	id := publish(t, d, "u", events.TypeScheduleUpdated, map[string]string{"schedule_id": "s1"})
	var typ, payload string
	if err := d.Pool.QueryRow(ctx, `SELECT type, payload::text FROM events WHERE id = $1`, id).Scan(&typ, &payload); err != nil {
		t.Fatal(err)
	}
	if typ != "schedule:updated" || payload != `{"schedule_id": "s1"}` {
		t.Errorf("stored %q %q", typ, payload)
	}
}

func TestPublishValidates(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		user    string
		typ     events.Type
		payload any
	}{
		"empty user":     {"", events.TypeInboxUpdated, map[string]string{}},
		"stream frame":   {"u", "heartbeat", map[string]string{}},
		"unknown type":   {"u", "nope", map[string]string{}},
		"array payload":  {"u", events.TypeInboxUpdated, []int{1}},
		"scalar payload": {"u", events.TypeInboxUpdated, "x"},
		"unmarshalable":  {"u", events.TypeInboxUpdated, make(chan int)},
	} {
		t.Run(name, func(t *testing.T) {
			err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
				_, err := events.Publish(ctx, tx, c.user, c.typ, c.payload)
				return err
			})
			if err == nil {
				t.Error("expected an error")
			}
		})
	}
	if n := count(t, d); n != 0 {
		t.Errorf("%d events stored", n)
	}
}

// Ids follow commit order even when transactions overlap, which is what makes "everything after
// id N" a safe cursor.
func TestPublishIdsFollowCommitOrder(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var committed []int64
	for range 12 {
		wg.Go(func() {
			err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
				id, err := events.Publish(ctx, tx, "u", events.TypeInboxUpdated, map[string]string{})
				if err != nil {
					return err
				}
				time.Sleep(5 * time.Millisecond) // hold the transaction open
				mu.Lock()
				committed = append(committed, id) // recorded just before COMMIT
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := 1; i < len(committed); i++ {
		if committed[i] < committed[i-1] {
			t.Fatalf("ids out of commit order: %v", committed)
		}
	}
}

func TestHubFiltersByUser(t *testing.T) {
	h := events.NewHub(quiet(), 4)
	h.MarkReady()
	a, _ := h.Subscribe("a")
	b, _ := h.Subscribe("b")
	h.Dispatch(events.Event{ID: 1, UserID: "a", Type: events.TypeInboxUpdated})
	select {
	case ev := <-a.C:
		if ev.ID != 1 {
			t.Errorf("a got %+v", ev)
		}
	default:
		t.Fatal("a did not receive its event")
	}
	select {
	case ev := <-b.C:
		t.Fatalf("b received another user's event: %+v", ev)
	default:
	}
}

// A subscriber that never reads cannot block Dispatch; it is dropped when its buffer fills, and
// the others are unaffected.
func TestHubDropsSlowConsumerWithoutBlocking(t *testing.T) {
	h := events.NewHub(quiet(), 3)
	h.MarkReady()
	slow, _ := h.Subscribe("u")
	fast, _ := h.Subscribe("u")

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := int64(1); i <= 10; i++ {
			h.Dispatch(events.Event{ID: i, UserID: "u"})
			select {
			case <-fast.C:
			default:
				t.Errorf("fast subscriber had no event %d", i)
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch blocked on a slow consumer")
	}
	select {
	case <-slow.Done():
	default:
		t.Fatal("slow consumer was not dropped")
	}
	if !errors.Is(slow.Err(), events.ErrSlowConsumer) {
		t.Errorf("slow.Err = %v", slow.Err())
	}
	if fast.Err() != nil || h.Subscribers() != 1 {
		t.Errorf("fast.Err = %v, subscribers = %d", fast.Err(), h.Subscribers())
	}
	slow.Close() // idempotent after a drop
}

func TestHubCloseEndsEverythingAndRefusesNew(t *testing.T) {
	h := events.NewHub(quiet(), 2)
	h.MarkReady()
	s, _ := h.Subscribe("u")
	h.Close()
	select {
	case <-s.Done():
	default:
		t.Fatal("subscription still open after Close")
	}
	if !errors.Is(s.Err(), events.ErrHubClosed) {
		t.Errorf("err = %v", s.Err())
	}
	if _, err := h.Subscribe("u"); !errors.Is(err, events.ErrHubClosed) {
		t.Errorf("Subscribe after Close = %v", err)
	}
}

// waitReady blocks until the source has its starting cursor and opened the hub.
func waitReady(t *testing.T, h *events.Hub) {
	t.Helper()
	select {
	case <-h.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("source never became ready")
	}
}

func runSource(t *testing.T, d *testdb.DB, h *events.Hub, wake <-chan struct{}, poll time.Duration) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &events.Source{Pool: d.Pool, Hub: h, Log: quiet(), Wake: wake, Poll: poll}
	go func() { defer close(done); _ = s.Run(ctx) }()
	return func() { cancel(); <-done }
}

func recv(t *testing.T, s *events.Subscription, within time.Duration) events.Event {
	t.Helper()
	select {
	case ev := <-s.C:
		return ev
	case <-time.After(within):
		t.Fatal("no event")
		return events.Event{}
	}
}

// Only the notification wakes the source (poll is an hour), then a dropped listener's catch-up
// wake finds events published while nobody was listening.
func TestSourceDispatchesOnWakeAndCatchesUpAfterAGap(t *testing.T) {
	d := testdb.New(t)
	h := events.NewHub(quiet(), 16)
	wake := make(chan struct{}, 1)
	stop := runSource(t, d, h, wake, time.Hour)
	defer stop()
	waitReady(t, h)
	sub, _ := h.Subscribe("u")

	id1 := publish(t, d, "u", events.TypeInboxUpdated, map[string]string{})
	wake <- struct{}{}
	if ev := recv(t, sub, 5*time.Second); ev.ID != id1 || ev.Type != events.TypeInboxUpdated {
		t.Errorf("first event = %+v", ev)
	}

	// Published while the "listener" is down: no wake arrives...
	id2 := publish(t, d, "u", events.TypeTaskUpdated, map[string]string{"task_id": "t"})
	id3 := publish(t, d, "u", events.TypeWorkspaceUpdated, map[string]string{})
	select {
	case ev := <-sub.C:
		t.Fatalf("event %+v dispatched without a wake", ev)
	case <-time.After(300 * time.Millisecond):
	}
	// ...and the reconnect wake delivers both, in order.
	wake <- struct{}{}
	if ev := recv(t, sub, 5*time.Second); ev.ID != id2 {
		t.Errorf("catch-up event 1 = %+v, want %d", ev, id2)
	}
	if ev := recv(t, sub, 5*time.Second); ev.ID != id3 {
		t.Errorf("catch-up event 2 = %+v, want %d", ev, id3)
	}
}

func TestSourcePollIsTheBackstop(t *testing.T) {
	d := testdb.New(t)
	h := events.NewHub(quiet(), 16)
	stop := runSource(t, d, h, nil, 50*time.Millisecond)
	defer stop()
	waitReady(t, h)
	sub, _ := h.Subscribe("u")
	id := publish(t, d, "u", events.TypeInboxUpdated, map[string]string{})
	if ev := recv(t, sub, 5*time.Second); ev.ID != id {
		t.Errorf("event = %+v", ev)
	}
}

// End to end through the real notifier: Publish's pg_notify wakes the source without the poll.
func TestPublishWakesSourceThroughTheNotifier(t *testing.T) {
	d := testdb.New(t)
	h := events.NewHub(quiet(), 16)
	n := reconcile.NewNotifier(d.Pool, quiet())
	wake := n.Subscribe(events.Channel)
	// A second subscriber of the same channel is the probe. Its first wake is the notifier's
	// catch-up after LISTEN; its second can only come from a pg_notify, so waiting for it proves
	// the notification path reaches the notifier before the source (poll: 1 h) is asked to deliver.
	probe := n.Subscribe(events.Channel)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = n.Run(ctx) })
	stop := runSource(t, d, h, wake, time.Hour)
	defer func() { stop(); cancel(); wg.Wait() }()
	waitReady(t, h)
	sub, _ := h.Subscribe("u")
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("the notifier never subscribed")
	}

	id := publish(t, d, "u", events.TypeSessionMessage, map[string]string{"session_id": "s"})
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish did not notify the channel")
	}
	if ev := recv(t, sub, 5*time.Second); ev.ID != id || ev.Type != events.TypeSessionMessage {
		t.Errorf("event = %+v", ev)
	}
}

func TestPruneDeletesOldEventsAndRaisesTheFloor(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	old1 := publish(t, d, "u", events.TypeInboxUpdated, map[string]string{})
	old2 := publish(t, d, "u", events.TypeInboxUpdated, map[string]string{})
	if _, err := d.Pool.Exec(ctx, `UPDATE events SET created_at = now() - interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	fresh := publish(t, d, "u", events.TypeInboxUpdated, map[string]string{})
	n, err := events.Prune(ctx, d.Pool, time.Now().Add(-24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	var floor, remaining int64
	if err := d.Pool.QueryRow(ctx, `SELECT pruned_through FROM event_floor`).Scan(&floor); err != nil || floor != old2 {
		t.Errorf("floor = %d (%v), want %d (old1 %d)", floor, err, old2, old1)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT min(id) FROM events`).Scan(&remaining); err != nil || remaining != fresh {
		t.Errorf("remaining min id = %d, %v", remaining, err)
	}
	// Nothing left to prune is a no-op that keeps the floor.
	if n, err := events.Prune(ctx, d.Pool, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
		t.Errorf("second Prune = %d, %v", n, err)
	}
}

func TestHubRefusesSubscribersUntilReady(t *testing.T) {
	h := events.NewHub(quiet(), 4)
	if _, err := h.Subscribe("u"); !errors.Is(err, events.ErrHubNotReady) {
		t.Fatalf("Subscribe before ready = %v, want ErrHubNotReady", err)
	}
	h.MarkReady()
	h.MarkReady() // idempotent
	if _, err := h.Subscribe("u"); err != nil {
		t.Fatalf("Subscribe after ready: %v", err)
	}
}

// A write that fails after publishing rolls the event back with it, and InTxPublish publishes
// what the domain function returns, after it.
func TestInTxPublishPublishesAfterTheDomainWork(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var order []string
	err := events.InTxPublish(ctx, d.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		order = append(order, "domain")
		return []events.Draft{{UserID: "u", Type: events.TypeInboxUpdated, Payload: map[string]string{}}}, nil
	})
	if err != nil || len(order) != 1 {
		t.Fatalf("InTxPublish = %v", err)
	}
	boom := errors.New("boom")
	err = events.InTxPublish(ctx, d.Pool, func(pgx.Tx) ([]events.Draft, error) {
		return []events.Draft{{UserID: "u", Type: events.TypeInboxUpdated, Payload: map[string]string{}}}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil || n != 1 {
		t.Errorf("events stored = %d (%v), want 1: the failed change must leave none", n, err)
	}
}
