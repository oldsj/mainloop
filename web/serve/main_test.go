package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>app</title>")},
		"theme-init.js":       {Data: []byte("/* theme */")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
	}
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestRootReturnsIndex(t *testing.T) {
	rec := get(t, newHandler(testFS(), ""), http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != revalidate {
		t.Fatalf("Cache-Control = %q, want %q", got, revalidate)
	}
}

func TestClientRoutesFallBackToIndex(t *testing.T) {
	h := newHandler(testFS(), "")
	for _, target := range []string{"/sessions/abc", "/projects/p1?view=compact", "/health"} {
		rec := get(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
			t.Errorf("%s: status %d body %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestHashedAssetsAreImmutable(t *testing.T) {
	rec := get(t, newHandler(testFS(), ""), http.MethodGet, "/assets/index-abc.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != immutableCache {
		t.Fatalf("Cache-Control = %q, want %q", got, immutableCache)
	}
}

func TestMissingAssetsAre404NotIndex(t *testing.T) {
	h := newHandler(testFS(), "")
	for _, target := range []string{"/assets/gone-123.js", "/missing.png"} {
		if rec := get(t, h, http.MethodGet, target); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, rec.Code)
		}
	}
}

func TestUnhashedFilesRevalidate(t *testing.T) {
	rec := get(t, newHandler(testFS(), ""), http.MethodGet, "/theme-init.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != revalidate {
		t.Fatalf("status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestPathTraversalStaysInsideTheDirectory(t *testing.T) {
	h := newHandler(testFS(), "")
	// Cleaned to a client route, so the app shell answers; nothing outside the directory is read.
	rec := get(t, h, http.MethodGet, "/../../etc/passwd")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, http.MethodGet, "/../go.mod"); rec.Code != http.StatusNotFound {
		t.Fatalf("go.mod status = %d, want 404", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := get(t, newHandler(testFS(), "https://api.example.test/v1"), http.MethodGet, "/")
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"connect-src 'self' https://api.example.test wss://api.example.test;",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

// The app uses the system's fonts and its own stylesheet: nothing is loaded from a font service, so
// none is allowed.
func TestStylesAndFontsAreSelfOnly(t *testing.T) {
	csp := contentSecurityPolicy("/api")
	for _, want := range []string{"; style-src 'self' 'unsafe-inline';", "; font-src 'self';"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "fonts.g") {
		t.Errorf("CSP %q still allows a font service", csp)
	}
}

// The app serves its own monospace font from the hashed assets. The response must say it is a font,
// since nosniff is set and the base image has no MIME table to look the extension up in, and it is
// cached for good like any other hashed asset.
func TestServesItsOwnFontAsAFont(t *testing.T) {
	files := testFS()
	files["assets/GeistMono-Variable-abc.woff2"] = &fstest.MapFile{
		Data: append([]byte("wOF2\x00\x01\x00\x00"), make([]byte, 64)...),
	}
	files["licenses/GeistMono-OFL.txt"] = &fstest.MapFile{Data: []byte("SIL OPEN FONT LICENSE Version 1.1")}
	h := newHandler(files, "/api")

	rec := get(t, h, http.MethodGet, "/assets/GeistMono-Variable-abc.woff2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "font/woff2" {
		t.Errorf("Content-Type = %q, want font/woff2", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != immutableCache {
		t.Errorf("Cache-Control = %q, want %q", got, immutableCache)
	}

	// Its licence ships beside the app, as the Open Font License asks.
	if rec := get(t, h, http.MethodGet, "/licenses/GeistMono-OFL.txt"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "SIL OPEN FONT LICENSE") {
		t.Errorf("licence: status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// GitHub serves the project avatars the API returns from avatars.githubusercontent.com; without it
// in img-src every project page shows a broken image.
func TestImagesAllowGitHubAvatarsOnly(t *testing.T) {
	csp := contentSecurityPolicy("/api")
	if !strings.Contains(csp, "; img-src 'self' data: https://avatars.githubusercontent.com;") {
		t.Fatalf("CSP = %q", csp)
	}
}

// The live view opens a WebSocket to the API host; without the ws or wss origin Chrome blocks it
// and the view never attaches.
func TestAPIOriginAllowsItsWebSocket(t *testing.T) {
	if csp := contentSecurityPolicy("http://localhost:8000"); !strings.Contains(csp, "connect-src 'self' http://localhost:8000 ws://localhost:8000;") {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestRelativeAPIURLAddsNoOrigin(t *testing.T) {
	if csp := contentSecurityPolicy("/api"); !strings.Contains(csp, "connect-src 'self';") {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestOnlyReadMethods(t *testing.T) {
	h := newHandler(testFS(), "")
	if rec := get(t, h, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
	if rec := get(t, h, http.MethodHead, "/"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD status %d, body length %d", rec.Code, rec.Body.Len())
	}
}

func TestHealthz(t *testing.T) {
	if rec := get(t, newHandler(testFS(), ""), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHealthcheckProbe(t *testing.T) {
	srv := httptest.NewServer(newHandler(testFS(), ""))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	if !healthcheck(port) {
		t.Fatal("healthcheck failed against a running server")
	}
	srv.Close()
	if healthcheck(port) {
		t.Fatal("healthcheck passed with no server")
	}
}
