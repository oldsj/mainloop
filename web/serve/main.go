// Command serve is the static file server in the web image. It serves the Vite build with a
// single-page-app fallback, a Content-Security-Policy and long-lived caching for hashed assets.
//
// `serve -healthcheck` probes the running server on localhost and exits 0 or 1, for the image's
// HEALTHCHECK (distroless has no shell or curl).
//
// Configuration (environment):
//
//	PORT          listen port (default 3000)
//	WEB_DIR       directory holding index.html and assets/ (default "dist")
//	VITE_API_URL  the API base URL the build was made with; an absolute URL adds its origin to
//	              the CSP connect-src, a relative one (the default) adds nothing
package main

import (
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const (
	immutableCache = "public, max-age=31536000, immutable"
	revalidate     = "no-cache"
)

// contentSecurityPolicy returns the CSP for a build whose API lives at apiURL.
func contentSecurityPolicy(apiURL string) string {
	connect := "'self'"
	if u, err := url.Parse(apiURL); err == nil && u.Scheme != "" && u.Host != "" {
		connect += " " + u.Scheme + "://" + u.Host
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		// Radix positions popovers with inline styles; the font CSS comes from Google Fonts.
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
		"font-src 'self' https://fonts.gstatic.com",
		"img-src 'self' data:",
		"connect-src " + connect,
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

type server struct {
	files fs.FS
	csp   string
}

func newHandler(files fs.FS, apiURL string) http.Handler {
	return &server{files: files, csp: contentSecurityPolicy(apiURL)}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h := w.Header()
	h.Set("Content-Security-Policy", s.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")

	if r.URL.Path == "/healthz" {
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok\n"))
		}
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}

	if info, err := fs.Stat(s.files, name); err == nil && info.Mode().IsRegular() {
		s.serveFile(w, r, name)
		return
	}

	// A missing hashed asset must not turn into index.html: the browser would try to run HTML
	// as a script and hide the real problem (a stale page after a deploy).
	if strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
		http.NotFound(w, r)
		return
	}

	s.serveFile(w, r, "index.html")
}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.files.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	rs, ok := f.(readSeeker)
	if !ok {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", immutableCache)
	} else {
		// index.html, theme-init.js and other unhashed files: revalidate so a deploy shows up.
		w.Header().Set("Cache-Control", revalidate)
	}
	http.ServeContent(w, r, name, info.ModTime(), rs)
}

type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// healthcheck reports whether the server on port answers /healthz.
func healthcheck(port string) bool {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

func main() {
	probe := flag.Bool("healthcheck", false, "probe the running server and exit")
	flag.Parse()

	port := getenv("PORT", "3000")
	if *probe {
		if !healthcheck(port) {
			os.Exit(1)
		}
		return
	}

	dir := getenv("WEB_DIR", "dist")
	addr := ":" + port

	if _, err := os.Stat(dir + "/index.html"); err != nil {
		log.Fatalf("serve: %s/index.html not found: %v", dir, err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(os.DirFS(dir), os.Getenv("VITE_API_URL")),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("serve: %s on %s", dir, addr)
	log.Fatal(srv.ListenAndServe())
}
