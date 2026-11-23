package mcpserver_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

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

// fakeBrowser stands in for the headless browser: it returns a fixed image and records the shots.
type fakeBrowser struct {
	mu    sync.Mutex
	shots []screenshot.Shot
}

var fakePNG = []byte("\x89PNG\r\n\x1a\nfictional")

func (f *fakeBrowser) Capture(_ context.Context, s screenshot.Shot) (screenshot.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shots = append(f.shots, s)
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
