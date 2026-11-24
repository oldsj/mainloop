// Package screenshot captures a workspace preview in a dedicated headless browser (issue #249).
//
// The browser is a separate Chromium (headless-shell) that Mainloop drives over the Chrome DevTools
// Protocol. It reaches the preview the way the owner does, through the preview host and its
// sign-in: each capture runs in a fresh incognito browser context that holds one short-lived
// preview cookie for one preview host, and the context is disposed when the capture ends. The
// browser holds no other credential and keeps nothing between captures.
//
// A capture is held to its preview host. The browser's network peer is the gateway that serves
// every preview and other hostnames too, so the page must not be able to take the browser
// anywhere else: the tab fails every request to another origin (CDP Fetch interception), the
// browser context sends every other host to a proxy that does not exist (which also covers
// WebSockets, workers and loopback, which Fetch does not see), and a page that is not on the
// preview's origin before and after the capture is refused with [ErrLeftPreview].
package screenshot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// Viewport is a named device size.
type Viewport struct {
	Name   string
	Width  int64
	Height int64
	Scale  float64
	Mobile bool
}

// The viewports a capture may use.
var (
	Mobile  = Viewport{Name: "mobile", Width: 390, Height: 844, Scale: 2, Mobile: true}
	Desktop = Viewport{Name: "desktop", Width: 1440, Height: 900, Scale: 1}
)

// ViewportNamed returns the viewport called name.
func ViewportNamed(name string) (Viewport, bool) {
	switch name {
	case Mobile.Name:
		return Mobile, true
	case Desktop.Name:
		return Desktop, true
	}
	return Viewport{}, false
}

// MaxBytes is the largest image a capture returns; a larger one is refused rather than stored.
const MaxBytes = 5 << 20

// maxFullPageHeight bounds a full-page capture, in CSS pixels.
const maxFullPageHeight = 4000

// Shot is one capture: the page, the cookie that signs the browser in to its host, and the size.
type Shot struct {
	URL      string
	Cookie   *http.Cookie
	Viewport Viewport
	FullPage bool
}

// Image is a captured PNG.
type Image struct {
	Data   []byte
	Width  int64
	Height int64
	// Status is the HTTP status of the page's document, 0 when the browser reported none.
	Status int64
}

// ErrTooLarge is a capture over MaxBytes.
var ErrTooLarge = errors.New("screenshot is larger than the limit")

// ErrLeftPreview is a page that left its preview host: a redirect or a navigation to another
// origin. Nothing of the other origin was loaded or captured.
var ErrLeftPreview = errors.New("the page left the preview host")

// deadProxy is where the browser context sends every host but the preview's. Nothing listens on
// the discard port, so those connections fail.
const deadProxy = "http://127.0.0.1:9"

// Capturer takes screenshots.
type Capturer interface {
	Capture(ctx context.Context, s Shot) (Image, error)
}

// CDP is a [Capturer] that drives a remote headless browser at URL (http://host:9222 or
// ws://host:9222).
type CDP struct {
	URL string
	// Timeout bounds one capture, from connecting to the browser to the image; 30s when zero.
	Timeout time.Duration
	// Settle is how long the page may keep working after its load event before the capture, for
	// client-rendered apps; 750ms when zero.
	Settle time.Duration
}

// Capture loads s.URL in a new incognito browser context and returns a PNG of it.
func (c *CDP) Capture(ctx context.Context, s Shot) (Image, error) {
	if c.URL == "" {
		return Image{}, errors.New("screenshot: no browser configured")
	}
	if s.Cookie == nil {
		return Image{}, errors.New("screenshot: no preview cookie")
	}
	timeout, settle := c.Timeout, c.Settle
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if settle <= 0 {
		settle = 750 * time.Millisecond
	}
	home, err := url.Parse(s.URL)
	if err != nil || (home.Scheme != "http" && home.Scheme != "https") || home.Hostname() == "" {
		return Image{}, errors.New("screenshot: the page is not an http(s) URL")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	alloc, cancelAlloc := remote.NewAllocator(ctx, c.URL)
	defer cancelAlloc()
	// A new browser context per capture: its cookies and storage are its own, and it is disposed
	// when tab is cancelled. Every host but the preview's goes to a proxy that does not exist;
	// "<-loopback>" sends loopback there too, where the browser's own DevTools listen.
	tab, cancelTab := chromedp.NewContext(alloc, chromedp.WithNewBrowserContext(func(p *target.CreateBrowserContextParams) {
		p.ProxyServer = deadProxy
		p.ProxyBypassList = home.Hostname() + ";<-loopback>"
	}))
	defer cancelTab()

	v := s.Viewport
	var main cdp.FrameID
	if err := chromedp.Do(tab,
		chromedp.EmulateViewport(v.Width, v.Height, chromedp.EmulateScale(v.Scale), mobile(v.Mobile)),
		setCookie(s.URL, s.Cookie),
		chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
			f, err := mainFrame(ctx, t)
			if err == nil {
				main = f.ID
			}
			return err
		}),
	); err != nil {
		return Image{}, fmt.Errorf("screenshot: prepare browser: %w", err)
	}
	fence, err := holdTo(tab, home, main)
	if err != nil {
		return Image{}, fmt.Errorf("screenshot: prepare browser: %w", err)
	}
	res, err := chromedp.Run(tab, chromedp.NavigateResponse(s.URL))
	if err != nil {
		if fence.leftMain.Load() {
			return Image{}, ErrLeftPreview
		}
		return Image{}, fmt.Errorf("screenshot: load page: %w", err)
	}
	var status int64
	if res != nil {
		status = res.Status
	}
	if err := chromedp.Do(tab, chromedp.Sleep(settle)); err != nil {
		return Image{}, fmt.Errorf("screenshot: load page: %w", err)
	}
	if err := onOrigin(tab, home); err != nil {
		return Image{}, err
	}
	data, width, height, err := capture(tab, v, s.FullPage)
	if err != nil {
		return Image{}, fmt.Errorf("screenshot: capture: %w", err)
	}
	// The page may have moved while the image was taken: an image of anything but the preview is
	// dropped.
	if err := onOrigin(tab, home); err != nil {
		return Image{}, err
	}
	if fence.leftMain.Load() {
		return Image{}, ErrLeftPreview
	}
	if len(data) > MaxBytes {
		return Image{}, ErrTooLarge
	}
	return Image{Data: data, Width: width, Height: height, Status: status}, nil
}

// fence is the tab's request interception.
type fence struct {
	// leftMain is set when the main frame tried to load a document of another origin.
	leftMain atomic.Bool
}

// holdTo pauses every request of the tab and lets through only those to home's origin; the rest
// fail as blocked by the client. It returns once interception is on, before any navigation.
func holdTo(tab context.Context, home *url.URL, main cdp.FrameID) (*fence, error) {
	f := &fence{}
	// Subscribed before Fetch.enable, so no paused request is missed.
	paused := chromedp.Events(tab, fetch.RequestPaused)
	if err := chromedp.Do(tab, chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		_, err := cdp.Call(ctx, t, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}})
		return err
	})); err != nil {
		return nil, err
	}
	go func() {
		for ev, err := range paused {
			if err != nil {
				return
			}
			allowed := ev.Request != nil && sameOrigin(ev.Request.URL, home)
			if !allowed && ev.ResourceType == network.ResourceTypeDocument && ev.FrameID == main {
				f.leftMain.Store(true)
			}
			id := ev.RequestID
			_ = chromedp.Do(tab, chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
				if allowed {
					_, err := cdp.Call(ctx, t, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: id})
					return err
				}
				_, err := cdp.Call(ctx, t, fetch.FailRequest, fetch.FailRequestParams{RequestID: id, ErrorReason: network.ErrorReasonBlockedByClient})
				return err
			}))
		}
	}()
	return f, nil
}

// onOrigin refuses a main frame that is not on home's origin, an error page included.
func onOrigin(tab context.Context, home *url.URL) error {
	var at string
	if err := chromedp.Do(tab, chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		f, err := mainFrame(ctx, t)
		if err == nil {
			at = f.URL
		}
		return err
	})); err != nil {
		return fmt.Errorf("screenshot: read page: %w", err)
	}
	if !sameOrigin(at, home) {
		return ErrLeftPreview
	}
	return nil
}

// mainFrame is the tab's main frame.
func mainFrame(ctx context.Context, t *chromedp.Target) (*cdp.Frame, error) {
	tree, err := cdp.Call(ctx, t, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return nil, err
	}
	if tree.FrameTree == nil || tree.FrameTree.Frame == nil {
		return nil, errors.New("the browser reported no main frame")
	}
	return tree.FrameTree.Frame, nil
}

// sameOrigin reports whether raw is an http(s) URL on home's scheme, host and port.
func sameOrigin(raw string, home *url.URL) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == home.Scheme && u.Host == home.Host
}

func mobile(on bool) chromedp.EmulateViewportOption {
	return func(p1 *emulation.SetDeviceMetricsOverrideParams, p2 *emulation.SetTouchEmulationEnabledParams) {
		p1.Mobile = on
		p2.Enabled = on
		if on {
			p1.ScreenOrientation = &emulation.ScreenOrientation{Type: emulation.ScreenOrientationTypePortraitPrimary, Angle: 0}
		}
	}
}

// setCookie stores the preview cookie in this browser context for the preview URL, with the
// attributes the preview host would have set itself.
func setCookie(url string, c *http.Cookie) chromedp.Action[chromedp.Void] {
	return chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		secure, httpOnly := c.Secure, c.HttpOnly
		p := network.SetCookieParams{
			Name: c.Name, Value: c.Value, URL: url, Path: c.Path, Secure: &secure, HTTPOnly: &httpOnly,
			SameSite: network.CookieSameSiteLax,
		}
		if !c.Expires.IsZero() {
			p.Expires = cdp.TimeSinceEpoch(float64(c.Expires.Unix()))
		}
		_, err := cdp.Call(ctx, t, network.SetCookie, p)
		return err
	})
}

// capture takes a PNG of the viewport, or of the whole page up to maxFullPageHeight, and returns
// it with its size in device pixels.
func capture(tab context.Context, v Viewport, full bool) ([]byte, int64, int64, error) {
	return chromedpRun(tab, func(ctx context.Context, t *chromedp.Target) (shot, error) {
		w, h := float64(v.Width), float64(v.Height)
		params := page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng, FromSurface: ptr(true)}
		if full {
			m, err := cdp.Call(ctx, t, page.GetLayoutMetrics, cdp.Empty{})
			if err != nil {
				return shot{}, err
			}
			if m.CSSContentSize != nil && m.CSSContentSize.Height > h {
				h = min(m.CSSContentSize.Height, maxFullPageHeight)
			}
			params.CaptureBeyondViewport = ptr(true)
			params.Clip = &page.Viewport{X: 0, Y: 0, Width: w, Height: h, Scale: 1}
		}
		r, err := cdp.Call(ctx, t, page.CaptureScreenshot, params)
		if err != nil {
			return shot{}, err
		}
		return shot{data: r.Data, width: int64(w * v.Scale), height: int64(h * v.Scale)}, nil
	})
}

type shot struct {
	data          []byte
	width, height int64
}

func chromedpRun(tab context.Context, a chromedp.Action[shot]) ([]byte, int64, int64, error) {
	s, err := chromedp.Run(tab, a)
	return s.data, s.width, s.height, err
}

func ptr[T any](v T) *T { return &v }
