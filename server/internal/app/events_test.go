package app_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

const owner = "local-dev-user"

type sseEnv struct {
	d      *testdb.DB
	hub    *events.Hub
	server *httptest.Server
}

// newSSE serves the real owner API on a real listener over a migrated database, with the outbox
// source running (poll only; the notifier is covered in the events package).
func newSSE(t *testing.T, buffer int, heartbeat time.Duration) *sseEnv {
	t.Helper()
	d := testdb.New(t)
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := events.NewHub(log, buffer)
	srv := app.New(app.Deps{Config: cfg, Log: log, DB: d.Pool, Pool: d.Pool, Hub: hub})
	srv.SetHeartbeat(heartbeat)
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	src := &events.Source{Pool: d.Pool, Hub: hub, Log: log, Poll: 20 * time.Millisecond}
	go func() { defer close(done); _ = src.Run(ctx) }()
	t.Cleanup(func() {
		hub.Close()
		ts.Close()
		cancel()
		<-done
	})
	time.Sleep(100 * time.Millisecond) // the source reads its starting cursor
	return &sseEnv{d: d, hub: hub, server: ts}
}

func (e *sseEnv) publish(t *testing.T, user string, typ events.Type, payload any) int64 {
	t.Helper()
	var id int64
	err := db.InTx(context.Background(), e.d.Pool, func(tx pgx.Tx) (err error) {
		id, err = events.Publish(context.Background(), tx, user, typ, payload)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type frame struct {
	id    string
	event string
	data  string
}

type client struct {
	t      *testing.T
	resp   *http.Response
	frames chan frame
	cancel context.CancelFunc
}

func (e *sseEnv) connect(t *testing.T, lastEventID string) *client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.server.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.test"
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the cleanup below
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &client{t: t, resp: resp, frames: make(chan frame, 1024), cancel: cancel}
	go func() {
		defer close(c.frames)
		sc := bufio.NewScanner(resp.Body)
		var f frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.event != "" {
					c.frames <- f
				}
				f = frame{}
			case strings.HasPrefix(line, "id: "):
				f.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.data += line[6:]
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return c
}

func (c *client) next(within time.Duration) (frame, bool) {
	c.t.Helper()
	select {
	case f, ok := <-c.frames:
		return f, ok
	case <-time.After(within):
		c.t.Fatal("timed out waiting for a frame")
		return frame{}, false
	}
}

func (c *client) mustNext() frame {
	c.t.Helper()
	f, ok := c.next(10 * time.Second)
	if !ok {
		c.t.Fatal("stream ended")
	}
	return f
}

func (c *client) expectNoFrame(within time.Duration) {
	c.t.Helper()
	select {
	case f, ok := <-c.frames:
		if ok {
			c.t.Fatalf("unexpected frame %+v", f)
		}
		c.t.Fatal("stream ended unexpectedly")
	case <-time.After(within):
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSSEConnectedFirstThenLiveEventsThenHeartbeat(t *testing.T) {
	e := newSSE(t, 16, 300*time.Millisecond)
	before := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	c := e.connect(t, "")
	if h := c.resp.Header; c.resp.StatusCode != 200 || h.Get("Content-Type") != "text/event-stream" ||
		h.Get("Cache-Control") != "no-cache" || h.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("response = %d %v", c.resp.StatusCode, h)
	}
	f := c.mustNext()
	if f.event != "connected" || f.id != itoa(before) {
		t.Fatalf("first frame = %+v, want connected with id %d", f, before)
	}
	var conn struct {
		UserID    string `json:"user_id"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(f.data), &conn); err != nil || conn.UserID != owner || conn.Timestamp == "" {
		t.Fatalf("connected data = %q (%v)", f.data, err)
	}
	// Events from before the connection are not replayed on a fresh connect.

	id := e.publish(t, owner, events.TypeScheduleUpdated, map[string]string{"schedule_id": "s1"})
	f = c.mustNext()
	if f.event != "schedule:updated" || f.id != itoa(id) || f.data != `{"schedule_id": "s1"}` {
		t.Fatalf("live frame = %+v", f)
	}
	// Silence for the heartbeat interval: a heartbeat with no id line.
	f = c.mustNext()
	if f.event != "heartbeat" || f.id != "" {
		t.Fatalf("heartbeat = %+v", f)
	}
	var beat struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(f.data), &beat); err != nil || beat.Timestamp == "" {
		t.Errorf("heartbeat data = %q (%v)", f.data, err)
	}
}

func TestSSEHeartbeatWaitsOutTheSilence(t *testing.T) {
	e := newSSE(t, 16, 500*time.Millisecond)
	c := e.connect(t, "")
	c.mustNext() // connected
	e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	time.Sleep(300 * time.Millisecond)
	if f := c.mustNext(); f.event != "inbox:updated" {
		t.Fatalf("frame = %+v", f)
	}
	// 300ms in, an event reset the clock, so a heartbeat is still ~200ms+ away at least.
	start := time.Now()
	if f := c.mustNext(); f.event != "heartbeat" {
		t.Fatalf("frame = %+v", f)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("heartbeat came %v after the last event; the interval restarts at each event", waited)
	}
}

func TestSSEResumeReplaysMissedEventsThenGoesLive(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	e1 := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	e2 := e.publish(t, owner, events.TypeTaskUpdated, map[string]string{"task_id": "t"})
	e3 := e.publish(t, owner, events.TypeWorkspaceUpdated, map[string]string{})
	e.publish(t, "someone-else", events.TypeInboxUpdated, map[string]string{})

	c := e.connect(t, itoa(e1))
	if f := c.mustNext(); f.event != "connected" || f.id != itoa(e1) {
		t.Fatalf("connected = %+v (the resume point stays the client's)", f)
	}
	for _, want := range []struct {
		id  int64
		typ string
	}{{e2, "task:updated"}, {e3, "workspace:updated"}} {
		if f := c.mustNext(); f.id != itoa(want.id) || f.event != want.typ {
			t.Fatalf("replayed %+v, want %d %s", f, want.id, want.typ)
		}
	}
	live := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	if f := c.mustNext(); f.id != itoa(live) {
		t.Fatalf("live after replay = %+v, want %d (the other user's event must not appear)", f, live)
	}
	c.expectNoFrame(200 * time.Millisecond)
}

// Events published while a client connects with Last-Event-ID arrive exactly once, in order.
func TestSSEResumeHasNoGapOrDuplicateUnderConcurrentPublishing(t *testing.T) {
	e := newSSE(t, 256, time.Hour)
	first := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	var (
		wg   sync.WaitGroup
		last int64
	)
	wg.Go(func() {
		for range 150 {
			last = e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
			time.Sleep(time.Millisecond)
		}
	})
	time.Sleep(40 * time.Millisecond)
	c := e.connect(t, itoa(first))
	wg.Wait()

	if f := c.mustNext(); f.event != "connected" {
		t.Fatalf("first = %+v", f)
	}
	want := first + 1
	for want <= last {
		f := c.mustNext()
		if f.id != itoa(want) {
			t.Fatalf("got id %s, want %d (gap or duplicate)", f.id, want)
		}
		want++
	}
	c.expectNoFrame(200 * time.Millisecond)
}

// Beyond the retained window, or ahead of the outbox, a resume is a fresh connection: nothing is
// replayed and `connected` tells the client to refetch.
func TestSSEResumeOutsideTheWindowStartsFresh(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	ctx := context.Background()
	old := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	mid := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	if _, err := e.d.Pool.Exec(ctx, `UPDATE events SET created_at = now() - interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	newest := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	if n, err := events.Prune(ctx, e.d.Pool, time.Now().Add(-24*time.Hour)); err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v", n, err)
	}

	for name, last := range map[string]string{
		"pruned":          itoa(old),
		"ahead of outbox": itoa(newest + 1000),
		"not a number":    "abc",
		"negative":        "-5",
	} {
		t.Run(name, func(t *testing.T) {
			c := e.connect(t, last)
			if f := c.mustNext(); f.event != "connected" || f.id != itoa(newest) {
				t.Fatalf("connected = %+v, want id %d", f, newest)
			}
			c.expectNoFrame(150 * time.Millisecond)
		})
	}
	// The newest pruned id itself is still a valid resume point: the client has seen everything
	// up to it.
	c := e.connect(t, itoa(mid))
	if f := c.mustNext(); f.event != "connected" || f.id != itoa(mid) {
		t.Fatalf("connected = %+v", f)
	}
	if f := c.mustNext(); f.id != itoa(newest) {
		t.Fatalf("replay = %+v, want %d", f, newest)
	}
}

func TestSSEOnlyDeliversTheUsersOwnEvents(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	c := e.connect(t, "")
	c.mustNext()
	e.publish(t, "someone-else", events.TypeInboxUpdated, map[string]string{})
	c.expectNoFrame(200 * time.Millisecond)
}

// Server shutdown: closing the hub ends open streams (so http.Server.Shutdown does not wait on
// them), and new streams are refused.
func TestSSEStreamsEndOnShutdown(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	a, b := e.connect(t, ""), e.connect(t, "")
	a.mustNext()
	b.mustNext()
	e.hub.Close()
	for _, c := range []*client{a, b} {
		if _, ok := c.next(5 * time.Second); ok {
			t.Fatal("got a frame after shutdown")
		}
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.server.URL+"/events", nil)
	req.Host = "api.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("stream during shutdown = %d, want 503", resp.StatusCode)
	}
}

func TestSSEEndsWhenTheClientLeaves(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	c := e.connect(t, "")
	c.mustNext()
	if e.hub.Subscribers() != 1 {
		t.Fatalf("subscribers = %d", e.hub.Subscribers())
	}
	c.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for e.hub.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscription leaked after the client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEventsWithoutAHubAnswer503(t *testing.T) {
	h := newHandler(t, nil, up())
	r := send(h, "GET", "/events", "", nil)
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
}
