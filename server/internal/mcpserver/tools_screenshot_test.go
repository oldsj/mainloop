package mcpserver_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/screenshot"
)

// fakeSigner stands in for the preview signer (internal/preview tests the real one): port 3000 is
// the only declared port.
type fakeSigner struct{}

func (fakeSigner) Visit(_ context.Context, ws string, port int, path string) (preview.Visit, error) {
	if port != 3000 {
		return preview.Visit{}, preview.ErrPortNotDeclared
	}
	return preview.Visit{
		URL:    "https://3000--" + ws + "--preview.example.test" + path,
		Cookie: &http.Cookie{Name: "__Host-mainloop-preview", Value: "signed", Path: "/", Secure: true, HttpOnly: true},
	}, nil
}

// fakeBrowser stands in for the headless browser: it returns a fixed image (or err) and records
// the shots. With hold set, a capture signals entered and waits for hold to close, as a page
// that hangs would.
type fakeBrowser struct {
	mu      sync.Mutex
	shots   []screenshot.Shot
	err     error
	hold    chan struct{}
	entered chan struct{}
}

var fakePNG = []byte("\x89PNG\r\n\x1a\nfictional")

func (f *fakeBrowser) Capture(_ context.Context, s screenshot.Shot) (screenshot.Image, error) {
	f.mu.Lock()
	f.shots = append(f.shots, s)
	hold, entered, err := f.hold, f.entered, f.err
	f.mu.Unlock()
	if hold != nil {
		entered <- struct{}{}
		<-hold
	}
	if err != nil {
		return screenshot.Image{}, err
	}
	return screenshot.Image{Data: fakePNG, Width: s.Viewport.Width * int64(s.Viewport.Scale), Height: s.Viewport.Height * int64(s.Viewport.Scale), Status: 200}, nil
}

func (f *fakeBrowser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shots)
}

func newScreenshotHarness(t *testing.T, b *fakeBrowser, on bool, perMinute int) *harness {
	t.Helper()
	return buildHarness(t, false, false, func(srv *mcpserver.Server, _ *pgxpool.Pool) {
		srv.Previews, srv.Screenshots, srv.ScreenshotsPerMinute = fakeSigner{}, b, perMinute
		srv.Policy.Screenshots = on
	})
}

func TestPreviewScreenshotIsOnlyAWorkspaceAgentsTool(t *testing.T) {
	h := newScreenshotHarness(t, &fakeBrowser{}, true, 0)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	if !slices.Contains(h.list(t, ordinary, ws.Token), "preview_screenshot") {
		t.Error("a workspace agent does not list preview_screenshot")
	}
	if slices.Contains(h.list(t, ordinary, main.Token), "preview_screenshot") {
		t.Error("the main thread lists preview_screenshot")
	}
	off := newScreenshotHarness(t, &fakeBrowser{}, false, 0)
	ws = off.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if slices.Contains(off.list(t, ordinary, ws.Token), "preview_screenshot") {
		t.Error("preview_screenshot is listed without a browser")
	}
}

func TestPreviewScreenshotCapturesTheSignedInPreviewOnce(t *testing.T) {
	b := &fakeBrowser{}
	h := newScreenshotHarness(t, b, true, 0)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	args := map[string]any{"request_id": "shot-1", "port": 3000, "path": "/menu?day=1", "viewports": []string{"mobile", "desktop"}}

	_, r := h.call(ordinary, ws.Token, "preview_screenshot", args)
	if r.Result.IsError {
		t.Fatalf("preview_screenshot = %q", r.text())
	}
	// Then the browser opened the workspace's own preview, signed in with its cookie, per viewport
	if b.count() != 2 {
		t.Fatalf("captures = %d, want 2", b.count())
	}
	for _, s := range b.shots {
		if s.URL != "https://3000--"+ws.SessionID+"--preview.example.test/menu?day=1" || s.Cookie == nil || s.Cookie.Value != "signed" {
			t.Errorf("shot = %q %v", s.URL, s.Cookie)
		}
	}
	// And the agent gets the images and their ids
	images := 0
	for _, c := range r.Result.Content {
		if c["type"] == "image" && c["mimeType"] == "image/png" {
			images++
		}
	}
	ids, _ := r.Result.StructuredContent["screenshot_ids"].([]any)
	if images != 2 || len(ids) != 2 || !strings.Contains(r.text(), "screenshot_ids") {
		t.Errorf("result: %d images, ids %v, text %q", images, ids, r.text())
	}

	// A retry returns what was stored and captures nothing
	_, again := h.call(ordinary, ws.Token, "preview_screenshot", args)
	if again.Result.IsError || again.Result.StructuredContent["replayed"] != true || b.count() != 2 {
		t.Errorf("replay = %v %q, captures %d", again.Result.StructuredContent, again.text(), b.count())
	}
	// The same request id for another page is a conflict
	args["path"] = "/other"
	if _, r := h.call(ordinary, ws.Token, "preview_screenshot", args); !r.Result.IsError || !strings.HasPrefix(r.text(), "[request_conflict]") {
		t.Errorf("conflict = %v %q", r.Result.IsError, r.text())
	}
}

func TestPreviewScreenshotRefusals(t *testing.T) {
	b := &fakeBrowser{}
	h := newScreenshotHarness(t, b, true, 2)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"request_id": "a", "port": 4000}, "[port_not_declared]"},
		{map[string]any{"request_id": "b", "port": 3000, "viewports": []string{"tablet"}}, "[validation]"},
		{map[string]any{"request_id": "c", "port": 3000, "viewports": []string{"mobile", "mobile"}}, "[validation]"},
		{map[string]any{"request_id": "d", "port": 0}, "[validation]"},
	}
	for _, c := range cases {
		if _, r := h.call(ordinary, ws.Token, "preview_screenshot", c.args); !r.Result.IsError || !strings.HasPrefix(r.text(), c.want) {
			t.Errorf("%v = %v %q, want %s", c.args, r.Result.IsError, r.text(), c.want)
		}
	}
	if b.count() != 0 {
		t.Fatalf("a refused request captured %d", b.count())
	}
	// Two a minute: the third is refused before the browser is asked
	for i, want := range []bool{false, false, true} {
		_, r := h.call(ordinary, ws.Token, "preview_screenshot", map[string]any{"request_id": uuid.NewString(), "port": 3000})
		if r.Result.IsError != want || (want && !strings.HasPrefix(r.text(), "[rate_limited]")) {
			t.Errorf("shot %d = %v %q", i, r.Result.IsError, r.text())
		}
	}
	if b.count() != 2 {
		t.Errorf("captures = %d, want 2", b.count())
	}
}

func TestReportShowsOnlyTheAttemptsOwnScreenshots(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	other := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	task, attempt := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	own, foreign := seedScreenshot(t, h, sup), seedScreenshot(t, h, other)

	report := func(req string, ids ...string) rpc {
		_, r := h.call(ordinary, sup.Token, "report", map[string]any{
			"task_id": task, "attempt_id": attempt, "request_id": req, "summary": "the menu page", "outcome": "progress",
			"screenshot_ids": ids,
		})
		return r
	}
	if r := report("r1", foreign); !r.Result.IsError || r.text() != "[422] report_screenshots" {
		t.Errorf("foreign screenshot = %v %q", r.Result.IsError, r.text())
	}
	if r := report("r2", uuid.NewString()); !r.Result.IsError || r.text() != "[422] report_screenshots" {
		t.Errorf("unknown screenshot = %v %q", r.Result.IsError, r.text())
	}
	r := report("r3", own)
	if r.Result.IsError {
		t.Fatalf("own screenshot = %q", r.text())
	}
	if got, _ := r.Result.StructuredContent["screenshot_ids"].([]any); len(got) != 1 || got[0] != own {
		t.Errorf("report screenshot_ids = %v", r.Result.StructuredContent["screenshot_ids"])
	}
	// The same request without the screenshot is another report
	_, changed := h.call(ordinary, sup.Token, "report", map[string]any{
		"task_id": task, "attempt_id": attempt, "request_id": "r3", "summary": "the menu page", "outcome": "progress",
	})
	if !changed.Result.IsError || changed.text() != "[409] report_request_conflict" {
		t.Errorf("changed screenshots = %v %q", changed.Result.IsError, changed.text())
	}
}

func seedScreenshot(t *testing.T, h *harness, a authtest.Seeded) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := h.env.Pool.Exec(context.Background(), `INSERT INTO preview_screenshots
		(id, owner_id, session_id, request_id, port, path, viewport, media_type, width, height, data, size_bytes)
		VALUES ($1, $2, $3, $1, 3000, '/', 'mobile', 'image/png', 780, 1688, $4, $5)`,
		id, a.UserID, a.SessionID, fakePNG, len(fakePNG)); err != nil {
		t.Fatal(err)
	}
	return id
}

// Failed captures count against the limit as stored ones do: an agent cannot keep the browser
// busy with pages that fail.
func TestPreviewScreenshotCountsFailedCaptures(t *testing.T) {
	b := &fakeBrowser{err: errors.New("the page hung")}
	h := newScreenshotHarness(t, b, true, 2)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	for i, want := range []string{"[capture_failed]", "[capture_failed]", "[rate_limited]"} {
		_, r := h.call(ordinary, ws.Token, "preview_screenshot", map[string]any{"request_id": uuid.NewString(), "port": 3000})
		if !r.Result.IsError || !strings.HasPrefix(r.text(), want) {
			t.Errorf("shot %d = %v %q, want %s", i, r.Result.IsError, r.text(), want)
		}
	}
	if b.count() != 2 {
		t.Errorf("captures = %d, want 2", b.count())
	}
}

// One capture per workspace at a time: a second call while the first hangs is refused at once,
// and another workspace still gets the browser.
func TestPreviewScreenshotOneCallPerWorkspaceAtATime(t *testing.T) {
	b := &fakeBrowser{hold: make(chan struct{}), entered: make(chan struct{}, 2)}
	h := newScreenshotHarness(t, b, true, 0)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	other := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	shoot := func(token string) <-chan rpc {
		out := make(chan rpc, 1)
		go func() {
			_, r := h.call(ordinary, token, "preview_screenshot", map[string]any{"request_id": uuid.NewString(), "port": 3000})
			out <- r
		}()
		return out
	}
	first := shoot(ws.Token)
	<-b.entered
	_, busy := h.call(ordinary, ws.Token, "preview_screenshot", map[string]any{"request_id": uuid.NewString(), "port": 3000})
	if !busy.Result.IsError || !strings.HasPrefix(busy.text(), "[busy]") {
		t.Errorf("second call = %v %q, want [busy]", busy.Result.IsError, busy.text())
	}
	second := shoot(other.Token)
	<-b.entered // the other workspace reached the browser while the first still holds it
	close(b.hold)
	for _, c := range []<-chan rpc{first, second} {
		if r := <-c; r.Result.IsError {
			t.Errorf("held capture = %q", r.text())
		}
	}
}

// A page that leaves the preview host stores nothing and says why.
func TestPreviewScreenshotRefusesAPageThatLeftThePreview(t *testing.T) {
	h := newScreenshotHarness(t, &fakeBrowser{err: screenshot.ErrLeftPreview}, true, 0)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	_, r := h.call(ordinary, ws.Token, "preview_screenshot", map[string]any{"request_id": "away", "port": 3000})
	if !r.Result.IsError || !strings.HasPrefix(r.text(), "[left_preview]") {
		t.Errorf("result = %v %q", r.Result.IsError, r.text())
	}
	var n int
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM preview_screenshots WHERE session_id = $1`, ws.SessionID).Scan(&n); err != nil || n != 0 {
		t.Errorf("stored %d screenshots (%v)", n, err)
	}
}

// The retention sweep drops old images and a workspace's oldest unreported ones beyond the cap,
// keeps every row, and spares a reported screenshot from the cap. A replay of an expired request
// returns its facts without the image.
func TestScreenshotImagesExpire(t *testing.T) {
	h := newTaskHarness(t, false)
	ctx := context.Background()
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	task, attempt := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	age := func(id string, d time.Duration) {
		t.Helper()
		if _, err := h.env.Pool.Exec(ctx, `UPDATE preview_screenshots SET created_at = now() - make_interval(secs => $2) WHERE id = $1`, id, d.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	old := seedScreenshot(t, h, sup)
	age(old, screenshot.MaxImageAge+time.Hour)
	reported := seedScreenshot(t, h, sup)
	age(reported, 2*time.Hour)
	if _, r := h.call(ordinary, sup.Token, "report", map[string]any{
		"task_id": task, "attempt_id": attempt, "request_id": "shown", "summary": "the menu page", "outcome": "progress",
		"screenshot_ids": []string{reported},
	}); r.Result.IsError {
		t.Fatalf("report = %q", r.text())
	}
	oldest := seedScreenshot(t, h, sup)
	age(oldest, time.Hour)
	for range screenshot.KeepUnreported {
		seedScreenshot(t, h, sup)
	}

	if err := screenshot.RetainSweep(h.env.Pool).Run(ctx); err != nil {
		t.Fatal(err)
	}
	held := func(id string) bool {
		var data []byte
		var expired *time.Time
		if err := h.env.Pool.QueryRow(ctx, `SELECT data, expired_at FROM preview_screenshots WHERE id = $1`, id).Scan(&data, &expired); err != nil {
			t.Fatal(err)
		}
		return data != nil && expired == nil
	}
	if held(old) {
		t.Error("an image past the age limit is held")
	}
	if held(oldest) {
		t.Error("the oldest unreported image beyond the cap is held")
	}
	if !held(reported) {
		t.Error("a reported image was dropped by the cap")
	}
	var rows, images int
	if err := h.env.Pool.QueryRow(ctx, `SELECT count(*), count(data) FROM preview_screenshots WHERE session_id = $1`, sup.SessionID).Scan(&rows, &images); err != nil {
		t.Fatal(err)
	}
	if rows != screenshot.KeepUnreported+3 || images != screenshot.KeepUnreported+1 {
		t.Errorf("rows %d, images %d; want %d rows, %d images", rows, images, screenshot.KeepUnreported+3, screenshot.KeepUnreported+1)
	}
}

func TestPreviewScreenshotReplaysAnExpiredRequestWithoutItsImage(t *testing.T) {
	h := newScreenshotHarness(t, &fakeBrowser{}, true, 0)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	args := map[string]any{"request_id": "shot-1", "port": 3000}
	if _, r := h.call(ordinary, ws.Token, "preview_screenshot", args); r.Result.IsError {
		t.Fatalf("preview_screenshot = %q", r.text())
	}
	if _, err := h.env.Pool.Exec(context.Background(), `UPDATE preview_screenshots SET data = NULL, expired_at = now() WHERE session_id = $1`, ws.SessionID); err != nil {
		t.Fatal(err)
	}
	_, r := h.call(ordinary, ws.Token, "preview_screenshot", args)
	if r.Result.IsError || !strings.Contains(r.text(), "image expired") {
		t.Fatalf("replay = %v %q", r.Result.IsError, r.text())
	}
	for _, c := range r.Result.Content {
		if c["type"] == "image" {
			t.Error("an expired screenshot came back with an image")
		}
	}
}
