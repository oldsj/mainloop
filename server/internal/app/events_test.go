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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/progress"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

const owner = "local-dev-user"

type sseEnv struct {
	d      *testdb.DB
	hub    *events.Hub
	server *httptest.Server
	srv    *app.Server
	log    *slog.Logger
	live   *progress.Tracker
}

// newSSE serves the real owner API on a real listener over a migrated database, with the outbox
// source running (poll only; the notifier is covered in the events package). It returns once the
// source has opened the hub.
func newSSE(t *testing.T, buffer int, heartbeat time.Duration) *sseEnv {
	t.Helper()
	e := newSSEWithoutSource(t, buffer, heartbeat)
	e.startSource(t)
	return e
}

// newSSEWithoutSource is newSSE with the source not started: the hub is not ready.
func newSSEWithoutSource(t *testing.T, buffer int, heartbeat time.Duration) *sseEnv {
	t.Helper()
	d := testdb.New(t)
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := events.NewHub(log, buffer)
	deps := apptest.Deps(t, cfg, log, d.Pool)
	live := progress.New(hub, progress.WithInterval(0))
	deps.DB, deps.Hub, deps.Live = d.Pool, hub, live
	srv := app.New(deps)
	srv.SetHeartbeat(heartbeat)
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(apicheck.Wrap(t, h))
	e := &sseEnv{d: d, hub: hub, server: ts, srv: srv, log: log, live: live}
	t.Cleanup(func() {
		hub.Close()
		ts.Close()
	})
	return e
}

// startSource runs the outbox source and waits until it has opened the hub.
func (e *sseEnv) startSource(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	src := &events.Source{Pool: e.d.Pool, Hub: e.hub, Log: e.log, Poll: 20 * time.Millisecond}
	go func() { defer close(done); _ = src.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-e.hub.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("source never became ready")
	}
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

// Streams are refused until the source has its cursor. Before, a stream that connected and then
// saw the source start would miss every event committed in between: not dispatched (the source
// starts after them) and not replayed (the stream began before them).
func TestSSERefusesStreamsUntilTheSourceIsReady(t *testing.T) {
	e := newSSEWithoutSource(t, 16, time.Hour)
	get := func() int {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.server.URL+"/events", nil)
		req.Host = "api.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("before the source is ready: status = %d, want 503", got)
	}
	missed := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	e.startSource(t)

	// A client that retried with the id it last saw gets what it missed, then live events.
	c := e.connect(t, itoa(missed-1))
	if f := c.mustNext(); f.event != "connected" {
		t.Fatalf("first frame = %+v", f)
	}
	if f := c.mustNext(); f.id != itoa(missed) {
		t.Fatalf("replay = %+v, want the event published before the source started (%d)", f, missed)
	}
	later := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	if f := c.mustNext(); f.id != itoa(later) {
		t.Fatalf("live = %+v, want %d", f, later)
	}
}

// Retention can overtake a replay in progress. The stream ends instead of going live with a hole,
// and the client's next attempt (its resume point is now below the floor) starts fresh.
func TestSSEEndsWhenPruningOvertakesTheReplay(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	first := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	var ids []int64
	for range 5 {
		ids = append(ids, e.publish(t, owner, events.TypeInboxUpdated, map[string]string{}))
	}
	var once sync.Once
	e.srv.SetReplay(2, func() {
		once.Do(func() {
			ctx := context.Background()
			if _, err := e.d.Pool.Exec(ctx, `UPDATE events SET created_at = now() - interval '3 days'`); err != nil {
				t.Error(err)
			}
			if _, err := events.Prune(ctx, e.d.Pool, time.Now().Add(-24*time.Hour)); err != nil {
				t.Error(err)
			}
		})
	})

	c := e.connect(t, itoa(first))
	if f := c.mustNext(); f.event != "connected" || f.id != itoa(first) {
		t.Fatalf("connected = %+v", f)
	}
	for _, id := range ids[:2] {
		if f := c.mustNext(); f.id != itoa(id) {
			t.Fatalf("replay = %+v, want %d", f, id)
		}
	}
	if f, ok := c.next(5 * time.Second); ok {
		t.Fatalf("got %+v; the stream should have ended when the floor passed the replay", f)
	}

	again := e.connect(t, itoa(first))
	if f := again.mustNext(); f.event != "connected" || f.id != itoa(ids[len(ids)-1]) {
		t.Fatalf("reconnect = %+v, want a fresh start at %d", f, ids[len(ids)-1])
	}
}

func TestSSELiveProgressFramesCarryNoIDAndASnapshotFollowsConnected(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	ref := progress.Ref{UserID: owner, SessionID: "s1", MessageID: "m1", Started: time.Now()}
	stored := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})

	// A client that joins mid-turn is told the state as it stands, right after connected.
	e.live.Observe(ref, &a2a.Task{ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("so far")}}}})
	c := e.connect(t, "")
	if f := c.mustNext(); f.event != "connected" {
		t.Fatalf("first frame %+v", f)
	}
	f := c.mustNext()
	if f.event != "session:progress" || f.id != "" || !strings.Contains(f.data, `"text":"so far"`) ||
		!strings.Contains(f.data, `"active":true`) {
		t.Fatalf("snapshot frame %+v", f)
	}

	// Live frames follow with no id, so they never move the resume point; stored events keep theirs.
	e.live.Observe(ref, &a2a.TaskArtifactUpdateEvent{Append: true, Artifact: &a2a.Artifact{
		ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(" and more")}}})
	f = c.mustNext()
	if f.event != "session:progress" || f.id != "" || !strings.Contains(f.data, `"text":"so far and more"`) {
		t.Fatalf("live frame %+v", f)
	}
	id := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	if f = c.mustNext(); f.event != "inbox:updated" || f.id != itoa(id) || id <= stored {
		t.Fatalf("stored frame %+v (id %d)", f, id)
	}
	e.live.End(ref, "reply-1")
	f = c.mustNext()
	if f.event != "session:progress" || f.id != "" || !strings.Contains(f.data, `"active":false`) ||
		!strings.Contains(f.data, `"reply_id":"reply-1"`) {
		t.Fatalf("closing frame %+v", f)
	}
}

func TestSSELiveProgressIsNotReplayedAfterAReconnect(t *testing.T) {
	e := newSSE(t, 16, time.Hour)
	ref := progress.Ref{UserID: owner, SessionID: "s1", MessageID: "m1", Started: time.Now()}
	e.live.Begin(ref)
	e.live.End(ref, "")
	last := e.publish(t, owner, events.TypeInboxUpdated, map[string]string{})
	c := e.connect(t, itoa(last-1))
	if f := c.mustNext(); f.event != "connected" {
		t.Fatalf("first frame %+v", f)
	}
	if f := c.mustNext(); f.event != "inbox:updated" || f.id != itoa(last) {
		t.Fatalf("replayed frame %+v", f)
	}
	c.expectNoFrame(200 * time.Millisecond)
}
