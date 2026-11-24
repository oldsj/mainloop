package screenshot_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/screenshot"
)

// chrome starts a headless Chrome for the test and returns its DevTools URL. *.test resolves to
// 127.0.0.1, so two hostnames reach one local server the way two apps reach one gateway. It skips
// when no browser is installed, unless MAINLOOP_TEST_CHROME names one (CI does).
func chrome(t *testing.T) string {
	t.Helper()
	bin := config.TestChrome()
	required := bin != ""
	candidates := []string{bin}
	if !required {
		candidates = []string{"google-chrome", "chromium", "chromium-browser", "headless-shell",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	}
	var path string
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		if required {
			t.Fatalf("MAINLOOP_TEST_CHROME=%q is not installed", bin)
		}
		t.Skip("no Chrome to drive; set MAINLOOP_TEST_CHROME to run the fence tests")
	}
	// Not t.TempDir: Chrome's helper processes can still be writing the profile as the test ends,
	// which fails that directory's strict cleanup. This one is removed best-effort, after the
	// browser's process group is stopped (cleanups run last-registered first).
	profile, err := os.MkdirTemp("", "fence-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 20 {
			if os.RemoveAll(profile) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("left Chrome's profile %s behind", profile)
	})
	cmd := exec.CommandContext(t.Context(), path, //nolint:gosec // G204: the test's own browser
		"--headless=new", "--remote-debugging-port=0", "--user-data-dir="+profile,
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "--no-sandbox",
		"--host-resolver-rules=MAP *.test 127.0.0.1", "about:blank")
	// Chrome is a tree of processes: run it in its own group and stop the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			if _, ws, ok := strings.Cut(s.Text(), "DevTools listening on "); ok {
				found <- ws
				break
			}
		}
		for s.Scan() { // keep draining, so the browser never blocks on a full pipe
		}
	}()
	select {
	case ws := <-found:
		u, err := url.Parse(ws)
		if err != nil {
			t.Fatal(err)
		}
		return "http://" + u.Host
	case <-time.After(30 * time.Second):
		t.Fatal("Chrome did not start")
		return ""
	}
}

// gateway is one local server that answers for every hostname, as the preview gateway does, and
// records the hosts it was asked for.
type gateway struct {
	*httptest.Server
	port string

	mu      sync.Mutex
	hosts   []string
	cookied bool
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	_, g.port, _ = net.SplitHostPort(g.Listener.Addr().String())
	return g
}

func (g *gateway) url(host, path string) string { return "http://" + host + ":" + g.port + path }

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.Host)
	g.mu.Lock()
	g.hosts = append(g.hosts, host+" "+r.URL.Path)
	if host == "a.test" && r.URL.Path == "/" {
		if c, err := r.Cookie("preview"); err == nil && c.Value == "fictional" {
			g.cookied = true
		}
	}
	g.mu.Unlock()
	if host != "a.test" {
		_, _ = fmt.Fprint(w, "<h1>Another app on the gateway</h1>")
		return
	}
	other := g.url("b.test", "")
	w.Header().Set("Content-Type", "text/html")
	switch r.URL.Path {
	case "/redirect":
		http.Redirect(w, r, other+"/secret", http.StatusFound)
	case "/leave":
		_, _ = fmt.Fprintf(w, `<h1>Fictional Bakery</h1><script>setTimeout(() => { location.href = %q }, 50)</script>`, other+"/secret")
	case "/reach":
		// Every way a page reaches another host without navigating: an image, fetch, a worker's
		// fetch, a WebSocket, and loopback (where the browser's own DevTools listen). Last, a fetch
		// of its own host, which shows the script ran through the others.
		_, _ = fmt.Fprintf(w, `<h1>Fictional Bakery</h1><img src=%[1]q>
<script>
fetch(%[2]q).catch(() => {});
new Worker(URL.createObjectURL(new Blob(['fetch(%[3]s).catch(() => {})'], {type: "text/javascript"})));
try { new WebSocket(%[4]q) } catch (e) {}
fetch(%[5]q).catch(() => {});
fetch("/own").catch(() => {});
</script>`, other+"/img", other+"/fetch", fmt.Sprintf("%q", other+"/worker"),
			"ws://b.test:"+g.port+"/ws", "http://127.0.0.1:"+g.port+"/loopback")
	default:
		_, _ = fmt.Fprint(w, "<h1>Fictional Bakery</h1>")
	}
}

func (g *gateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.hosts)
}

func shot(g *gateway, path string) screenshot.Shot {
	return screenshot.Shot{
		URL:      g.url("a.test", path),
		Cookie:   &http.Cookie{Name: "preview", Value: "fictional", Path: "/", HttpOnly: true},
		Viewport: screenshot.Desktop,
	}
}

// A capture of the preview host works, and its page reaches nothing but that host.
func TestCaptureStaysOnThePreviewHost(t *testing.T) {
	c := &screenshot.CDP{URL: chrome(t), Settle: 1500 * time.Millisecond}
	g := newGateway(t)
	img, err := c.Capture(context.Background(), shot(g, "/reach"))
	if err != nil {
		t.Fatal(err)
	}
	if img.Status != http.StatusOK || len(img.Data) == 0 || img.Width != 1440 {
		t.Errorf("image = status %d, %d bytes, width %d", img.Status, len(img.Data), img.Width)
	}
	seen := g.seen()
	for _, h := range seen {
		if !strings.HasPrefix(h, "a.test ") {
			t.Errorf("the page reached %q", h)
		}
	}
	if !slices.Contains(seen, "a.test /own") {
		t.Errorf("the page's script did not run to its end: %v", seen)
	}

	// The preview cookie went to the preview host.
	if _, err := c.Capture(context.Background(), shot(g, "/")); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.cookied {
		t.Error("the preview host did not get the preview cookie")
	}
}

// A dev server that sends the browser to another host gets nothing back: no request reaches that
// host and the capture is refused.
func TestCaptureRefusesAPageThatLeavesThePreviewHost(t *testing.T) {
	c := &screenshot.CDP{URL: chrome(t), Settle: 500 * time.Millisecond}
	for _, path := range []string{"/redirect", "/leave"} {
		t.Run(path, func(t *testing.T) {
			g := newGateway(t)
			_, err := c.Capture(context.Background(), shot(g, path))
			if !errors.Is(err, screenshot.ErrLeftPreview) {
				t.Errorf("capture = %v, want ErrLeftPreview", err)
			}
			for _, h := range g.seen() {
				if !strings.HasPrefix(h, "a.test ") {
					t.Errorf("the page reached %q", h)
				}
			}
		})
	}
}
