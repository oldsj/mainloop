package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	previews "github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/screenshot"
)

// PreviewSigner signs Mainloop's headless browser in to one workspace preview host; the
// [previews.Proxy] from [previews.NewSigner] is the real one.
type PreviewSigner interface {
	Visit(ctx context.Context, workspaceID string, port int, path string) (previews.Visit, error)
}

const maxScreenshotPath = 2048

// maxCaptures is how many captures the one shared browser runs at once; a call waits up to
// captureWait for room. A hung page holds its place until the capture's own timeout.
const (
	maxCaptures = 2
	captureWait = 30 * time.Second
)

// captureGate bounds the captures in flight: one call per workspace at a time, and maxCaptures
// captures in all.
type captureGate struct {
	once   sync.Once
	slots  chan struct{}
	mu     sync.Mutex
	active map[string]bool
}

func (g *captureGate) init() {
	g.once.Do(func() {
		g.slots = make(chan struct{}, maxCaptures)
		g.active = map[string]bool{}
	})
}

// claim marks a workspace's call in flight; false when one already is.
func (g *captureGate) claim(workspaceID string) bool {
	g.init()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[workspaceID] {
		return false
	}
	g.active[workspaceID] = true
	return true
}

func (g *captureGate) release(workspaceID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.active, workspaceID)
}

// slot waits for room in the browser, at most captureWait; the returned func gives it back.
func (g *captureGate) slot(ctx context.Context) (func(), bool) {
	g.init()
	t := time.NewTimer(captureWait)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	case <-t.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// screenshotTools is preview_screenshot (docs/specs/workspaces.md "Preview screenshots"). The
// policy shows it only to workspace agents, and only when a browser is configured.
func (s *Server) screenshotTools() []Tool {
	return []Tool{{
		Name: agentauth.ToolPreviewScreenshot,
		Description: "Screenshot your workspace's running preview, as the owner would see it on a phone " +
			"(mobile, 390x844 at 2x) or a laptop (desktop, 1440x900). Mainloop's own headless browser opens " +
			"the preview of a declared port through its signed-in preview host and returns PNG images you " +
			"can look at. Each screenshot has an id: pass the ids as screenshot_ids to report to show them " +
			"to the owner in the task view and their conversation. Reuse request_id when retrying; a new " +
			"request_id takes new screenshots. The dev server must be running on the port. The page must " +
			"stay on its preview host: requests to other hosts fail, and a redirect or navigation away is " +
			"refused as left_preview. Images expire after 7 days.",
		InputSchema: objectSchema(map[string]any{
			"request_id": stringSchema(1, 100),
			"port":       map[string]any{"type": "integer", "minimum": 1, "maximum": 65535},
			"path": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxScreenshotPath, "default": "/",
				"description": "Path on the preview, starting with a single /, with any query.",
			},
			"viewports": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true, "default": []any{"mobile"},
				"items": map[string]any{"type": "string", "enum": []any{"mobile", "desktop"}},
			},
			"full_page": map[string]any{
				"type": "boolean", "default": false,
				"description": "Capture the whole page down to 4000 CSS pixels instead of the first screen.",
			},
		}, "request_id", "port"),
		Handle: s.previewScreenshot,
	}}
}

type screenshotRequest struct {
	RequestID string   `json:"request_id"`
	Port      int      `json:"port"`
	Path      string   `json:"path"`
	Viewports []string `json:"viewports"`
	FullPage  bool     `json:"full_page"`
}

func (in *screenshotRequest) validate() ([]screenshot.Viewport, error) {
	var err error
	if in.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return nil, err
	}
	if in.Port < 1 || in.Port > 65535 || len(in.Path) > maxScreenshotPath {
		return nil, errValidation
	}
	if in.Path == "" {
		in.Path = "/"
	}
	if len(in.Viewports) == 0 {
		in.Viewports = []string{screenshot.Mobile.Name}
	}
	if len(in.Viewports) > 2 {
		return nil, errValidation
	}
	vs := make([]screenshot.Viewport, 0, len(in.Viewports))
	for i, name := range in.Viewports {
		v, ok := screenshot.ViewportNamed(name)
		if !ok || slices.Contains(in.Viewports[:i], name) {
			return nil, errValidation
		}
		vs = append(vs, v)
	}
	return vs, nil
}

func (s *Server) previewScreenshot(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in screenshotRequest
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	viewports, err := in.validate()
	if err != nil {
		return Result{}, err
	}
	if s.Previews == nil || s.Screenshots == nil || s.Pool == nil {
		return Result{}, failure("unavailable", "preview screenshots are not available")
	}
	// The workspace agent's binding is its workspace.
	workspaceID := a.BindingID
	q := dbq.New(s.Pool)
	prior, err := q.ListPreviewScreenshotsByRequest(ctx, dbq.ListPreviewScreenshotsByRequestParams{SessionID: workspaceID, RequestID: in.RequestID})
	if err != nil {
		return Result{}, err
	}
	if len(prior) > 0 {
		if !sameRequest(prior, in) {
			return Result{}, failure("request_conflict", "request_id was already used for other screenshots")
		}
		return screenshotResult(prior, true), nil
	}
	if !s.shots.claim(workspaceID) {
		return Result{}, failure("busy", "another screenshot of this workspace is still being taken; wait for it, then try again")
	}
	defer s.shots.release(workspaceID)
	visit, err := s.Previews.Visit(ctx, workspaceID, in.Port, in.Path)
	switch {
	case errors.Is(err, previews.ErrNotPreviewable):
		return Result{}, failure("not_previewable", "this workspace has no preview Mainloop can open (it is not running, or the task attempt no longer owns it)")
	case errors.Is(err, previews.ErrPortNotDeclared):
		return Result{}, failure("port_not_declared", fmt.Sprintf("port %d is not a declared preview port of this workspace", in.Port))
	case errors.Is(err, previews.ErrBadPath):
		return Result{}, failure("validation", "path must start with a single / and have no control characters")
	case err != nil:
		return Result{}, err
	}
	// Every attempt counts against the limit, stored or not, so failed and hung captures count too.
	// The workspace's lock orders concurrent calls (another serve-mcp replica's included); the
	// transaction re-checks the caller's credential, so a revoked agent captures nothing.
	limit := s.screenshotsPerMinute()
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:preview-screenshot:"+workspaceID); err != nil {
			return err
		}
		q := dbq.New(tx)
		n, err := q.CountRecentScreenshotAttempts(ctx, workspaceID)
		if err != nil {
			return err
		}
		if int(n)+len(viewports) > limit {
			return failure("rate_limited", fmt.Sprintf("at most %d screenshots a minute; wait and try again", limit))
		}
		return q.InsertScreenshotAttempt(ctx, dbq.InsertScreenshotAttemptParams{
			SessionID: workspaceID, Captures: int32(len(viewports)), //nolint:gosec // 1 or 2
		})
	})
	if err != nil {
		return Result{}, err
	}
	done, ok := s.shots.slot(ctx)
	if !ok {
		return Result{}, failure("busy", "the screenshot browser is busy with other workspaces; try again in a minute")
	}
	defer done()
	rows := make([]dbq.InsertPreviewScreenshotParams, 0, len(viewports))
	for _, v := range viewports {
		img, err := s.Screenshots.Capture(ctx, screenshot.Shot{URL: visit.URL, Cookie: visit.Cookie, Viewport: v, FullPage: in.FullPage})
		if errors.Is(err, screenshot.ErrTooLarge) {
			return Result{}, failure("too_large", "the screenshot is larger than 5 MB; try without full_page")
		}
		if errors.Is(err, screenshot.ErrLeftPreview) {
			s.Log.WarnContext(ctx, "preview screenshot left the preview host", "session", workspaceID, "port", in.Port, "viewport", v.Name)
			return Result{}, failure("left_preview", "the page redirected or navigated away from the workspace's preview host; "+
				"screenshots show only the preview itself, so capture a path that stays on it")
		}
		if err != nil {
			s.Log.WarnContext(ctx, "preview screenshot failed", "session", workspaceID, "port", in.Port, "viewport", v.Name, "error", err)
			return Result{}, failure("capture_failed", "the browser could not open or capture the preview; check that the dev server answers on the port")
		}
		var status *int32
		if img.Status >= 100 && img.Status <= 599 {
			st := int32(img.Status) //nolint:gosec // an HTTP status
			status = &st
		}
		rows = append(rows, dbq.InsertPreviewScreenshotParams{
			ID: uuid.NewString(), OwnerID: a.UserID, SessionID: workspaceID, RequestID: in.RequestID,
			Port: int32(in.Port), Path: in.Path, Viewport: v.Name, FullPage: in.FullPage, //nolint:gosec // port is 1..65535
			MediaType: "image/png", Width: int32(img.Width), Height: int32(img.Height), //nolint:gosec // bounded by the viewport
			PageStatus: status, Data: img.Data, SizeBytes: int32(len(img.Data)), //nolint:gosec // at most screenshot.MaxBytes
		})
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, r := range rows {
			if err := dbq.New(tx).InsertPreviewScreenshot(ctx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	// A concurrent retry may have stored its own rows first: answer with what is stored.
	stored, err := q.ListPreviewScreenshotsByRequest(ctx, dbq.ListPreviewScreenshotsByRequestParams{SessionID: workspaceID, RequestID: in.RequestID})
	if err != nil {
		return Result{}, err
	}
	return screenshotResult(stored, false), nil
}

func (s *Server) screenshotsPerMinute() int {
	if s.ScreenshotsPerMinute > 0 {
		return s.ScreenshotsPerMinute
	}
	return 10
}

// sameRequest reports whether stored rows answer the request: the same port, path, page mode and
// viewports.
func sameRequest(rows []dbq.PreviewScreenshot, in screenshotRequest) bool {
	if len(rows) != len(in.Viewports) {
		return false
	}
	for _, r := range rows {
		if int(r.Port) != in.Port || r.Path != in.Path || r.FullPage != in.FullPage || !slices.Contains(in.Viewports, r.Viewport) {
			return false
		}
	}
	return true
}

func screenshotResult(rows []dbq.PreviewScreenshot, replayed bool) Result {
	var b strings.Builder
	items := make([]map[string]any, 0, len(rows))
	images := make([]Image, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		status := "no HTTP status"
		if r.PageStatus != nil {
			status = fmt.Sprintf("HTTP %d", *r.PageStatus)
		}
		expired := r.ExpiredAt != nil
		if expired {
			status += ", image expired"
		}
		fmt.Fprintf(&b, "screenshot %s: %s %dx%d of port %d %s (%s)\n", r.ID, r.Viewport, r.Width, r.Height, r.Port, r.Path, status)
		items = append(items, map[string]any{
			"id": r.ID, "viewport": r.Viewport, "port": r.Port, "path": r.Path, "full_page": r.FullPage,
			"width": r.Width, "height": r.Height, "page_status": r.PageStatus, "media_type": r.MediaType,
			"expired": expired,
		})
		if !expired {
			images = append(images, Image{MediaType: r.MediaType, Data: r.Data})
		}
		ids = append(ids, r.ID)
	}
	b.WriteString("To show them to the owner, pass these ids as screenshot_ids to report.")
	return Result{
		Text:       b.String(),
		Structured: map[string]any{"screenshots": items, "screenshot_ids": ids, "replayed": replayed},
		Images:     images,
	}
}
