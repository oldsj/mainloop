package webfetch_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/webfetch"
)

func TestPublicRefusesEveryNonPublicRange(t *testing.T) {
	deny, err := webfetch.ParseDeny(" 203.0.114.0/24 , ")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"93.184.215.14": true, "8.8.8.8": true, "2606:4700::6810:84e5": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.41.2": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "0.1.2.3": false,
		"255.255.255.255": false, "224.0.0.1": false, "240.0.0.1": false, "192.0.2.1": false,
		"198.18.0.1": false, "fd00:ec2::254": false, "fe80::1": false, "ff02::1": false, "::": false,
		// An IPv4 address in IPv6 clothing is judged as the IPv4 address.
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "64:ff9b::808:808": true,
		"2002:7f00:1::": false, "2001:db8::1": false,
		// An operator's own range.
		"203.0.114.9": false,
	}
	for s, want := range cases {
		if got := webfetch.Public(netip.MustParseAddr(s), deny); got != want {
			t.Errorf("Public(%s) = %v, want %v", s, got, want)
		}
	}
	if _, err := webfetch.ParseDeny("nope"); err == nil {
		t.Error("a malformed CIDR was accepted")
	}
}

func code(err error) string {
	var e *webfetch.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fmt.Sprintf("not a refusal: %v", err)
}

func TestFetchRefusesBadURLsBeforeConnecting(t *testing.T) {
	f := &webfetch.Fetcher{}
	cases := map[string]string{
		"ftp://example.com/":          webfetch.CodeInvalidURL,
		"/relative":                   webfetch.CodeInvalidURL,
		"http://user:pw@example.com/": webfetch.CodeInvalidURL,
		"http://example.com:8080/":    webfetch.CodeInvalidURL,
		"http://" + strings.Repeat("a", webfetch.MaxURL) + ".com/": webfetch.CodeInvalidURL,
		"http://10.0.0.1/":              webfetch.CodeBlockedAddress,
		"http://169.254.169.254/latest": webfetch.CodeBlockedAddress,
		"http://[::1]/":                 webfetch.CodeBlockedAddress,
		// A name that resolves to loopback is refused when it is dialed.
		"http://localhost/": webfetch.CodeBlockedAddress,
	}
	for u, want := range cases {
		if _, err := f.Fetch(context.Background(), u); code(err) != want {
			t.Errorf("Fetch(%q) = %v, want %s", u, err, want)
		}
	}
}

// local serves h and returns a fetcher allowed to reach it, and its URL with a host name.
func local(t *testing.T, h http.Handler) (*webfetch.Fetcher, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	f := &webfetch.Fetcher{Timeout: 2 * time.Second}
	ap := netip.MustParseAddrPort(u.Host)
	webfetch.AllowOnly(f, ap)
	// By name, as a real fetch is: the name is resolved and its address checked when dialed.
	return f, fmt.Sprintf("http://localhost:%d", ap.Port())
}

func TestFetchRendersHTMLAsText(t *testing.T) {
	// Given a page with a title, script, headings, a list and links
	f, base := local(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>Release  History</title><style>p{}</style></head>
<body><script>steal()</script><h1>Go releases</h1><p>Go 1.25 is   out.<br>See <a href="/doc/go1.25">the notes</a>.</p>
<ul><li>fast</li><li>safe</li></ul><a href="javascript:x()">no</a><pre>  keep
    this</pre></body></html>`)
	}))

	// When it is fetched
	page, err := f.Fetch(context.Background(), base+"/doc#frag")

	// Then the title and readable text come back, without the script, with links resolved
	if err != nil {
		t.Fatal(err)
	}
	if page.Status != 200 || page.ContentType != "text/html" || page.Title != "Release History" || page.FinalURL != base+"/doc" {
		t.Fatalf("page = %+v", page)
	}
	for _, want := range []string{"# Go releases", "Go 1.25 is out.\nSee [the notes](" + base + "/doc/go1.25) .", "- fast\n- safe", "  keep\n    this"} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, page.Text)
		}
	}
	if strings.Contains(page.Text, "steal") || strings.Contains(page.Text, "p{}") || strings.Contains(page.Text, "javascript") {
		t.Errorf("text carries script or style:\n%s", page.Text)
	}
}

func TestFetchRefusesARedirectToAPrivateAddress(t *testing.T) {
	// Given a public page that redirects to the cloud metadata address
	f, base := local(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))

	// When it is fetched, Then the hop is refused
	if _, err := f.Fetch(context.Background(), base+"/"); code(err) != webfetch.CodeBlockedAddress {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchStopsEndlessRedirects(t *testing.T) {
	f, base := local(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	if _, err := f.Fetch(context.Background(), base+"/"); code(err) != webfetch.CodeTooManyRedirects {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchCutsALongBodyAndRefusesBinary(t *testing.T) {
	f, base := local(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("é", 2000))) // 4000 bytes
	}))
	f.MaxBody = 1001 // cuts inside an é

	page, err := f.Fetch(context.Background(), base+"/long")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || page.Text != strings.Repeat("é", 500) {
		t.Fatalf("long: truncated %v, %d bytes", page.Truncated, len(page.Text))
	}
	page, err = f.Fetch(context.Background(), base+"/img")
	if code(err) != webfetch.CodeUnsupportedContent || page == nil || page.Status != 200 || page.Text != "" {
		t.Fatalf("image = %+v, %v", page, err)
	}
}

func TestFetchTimesOut(t *testing.T) {
	f, base := local(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	f.Timeout = 200 * time.Millisecond
	if _, err := f.Fetch(context.Background(), base+"/"); code(err) != webfetch.CodeTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestSlicePagesTextOnRuneAndLineBoundaries(t *testing.T) {
	text := "line one\nline two é\nline three"
	part, next := webfetch.Slice(text, 0, 15)
	if part != "line one\n" || next != 9 {
		t.Fatalf("first = %q, %d", part, next)
	}
	part, next = webfetch.Slice(text, next, 10) // would end inside é
	if part != "line two " || next != 18 {
		t.Fatalf("second = %q, %d", part, next)
	}
	if part, next = webfetch.Slice(text, 0, 100); part != text || next != -1 {
		t.Fatalf("whole = %q, %d", part, next)
	}
	if part, next = webfetch.Slice(text, 999, 10); part != "" || next != -1 {
		t.Fatalf("past the end = %q, %d", part, next)
	}
}
