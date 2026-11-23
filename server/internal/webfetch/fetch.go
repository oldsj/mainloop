package webfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Defaults of [Fetcher].
const (
	DefaultTimeout      = 15 * time.Second
	DefaultMaxBody      = 2 << 20
	DefaultMaxRedirects = 5
)

const userAgent = "Mainloop-WebFetch/1 (+https://github.com/oldsj/mainloop)"

// Fetcher makes guarded GET requests. The zero value uses the defaults; it is safe for
// concurrent use once in use.
type Fetcher struct {
	// Timeout bounds a whole fetch: every redirect, the headers and the body.
	Timeout time.Duration
	// MaxBody is the most body bytes read; a longer body is cut and marked Truncated.
	MaxBody int64
	// MaxRedirects is the most redirects followed.
	MaxRedirects int
	// Deny are ranges refused beyond the built-in non-public ones (MAINLOOP_WEB_FETCH_DENY_CIDRS).
	Deny []netip.Prefix

	// allow replaces the dial-time check and testPort is one more port a URL may name; only tests
	// set them, to reach a local server.
	allow    func(netip.AddrPort) bool
	testPort string
}

// Page is what one fetch read.
type Page struct {
	// URL is the URL asked for; FinalURL the one answered, after redirects.
	URL, FinalURL string
	Status        int
	ContentType   string
	Title         string
	// Text is the body as text: HTML rendered as plain text, other text types as they came.
	Text string
	// Truncated is set when the body was longer than MaxBody and was cut.
	Truncated bool
	// Bytes is how many body bytes were read.
	Bytes int
}

func (f *Fetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return DefaultTimeout
}

func (f *Fetcher) maxBody() int64 {
	if f.MaxBody > 0 {
		return f.MaxBody
	}
	return DefaultMaxBody
}

func (f *Fetcher) maxRedirects() int {
	if f.MaxRedirects > 0 {
		return f.MaxRedirects
	}
	return DefaultMaxRedirects
}

// dialAllowed is the dial-time check: the address actually being connected to must be public and
// on port 80 or 443.
func (f *Fetcher) dialAllowed(ap netip.AddrPort) bool {
	if f.allow != nil {
		return f.allow(ap)
	}
	return (ap.Port() == 80 || ap.Port() == 443) && Public(ap.Addr(), f.Deny)
}

func (f *Fetcher) client() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		// Control runs for every connection attempt with the resolved address, after DNS and
		// before connecting: a name that resolves to a private address, now or after a rebind,
		// is refused here, on the first request and on every redirect.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !f.dialAllowed(ap) {
				return errBlocked
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                  nil, // never a proxy from the environment
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
	}
	return &http.Client{
		Transport: transport,
		Jar:       nil, // no cookies
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > f.maxRedirects() {
				return refuse(CodeTooManyRedirects, "more than %d redirects", f.maxRedirects())
			}
			if e := checkURL(req.URL, f.Deny, f.testPort); e != nil {
				return e
			}
			return nil
		},
	}
}

// Fetch GETs rawURL and reads it as text. A refusal is an [*Error]. A page with any HTTP status is
// returned as read, so the caller sees a 404's text too. A body that is not text is refused with
// [CodeUnsupportedContent] and the page is returned alongside, without its text, so the caller
// can still record the status.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !u.IsAbs() {
		return nil, refuse(CodeInvalidURL, "not an absolute URL")
	}
	u.Fragment = ""
	if e := checkURL(u, f.Deny, f.testPort); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, refuse(CodeInvalidURL, "not a valid URL")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.1")
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, f.failure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	limit := f.maxBody()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, f.failure(ctx, err)
	}
	page := &Page{URL: u.String(), FinalURL: resp.Request.URL.String(), Status: resp.StatusCode}
	if int64(len(body)) > limit {
		body, page.Truncated = body[:limit], true
	}
	page.Bytes = len(body)

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	media, _, _ := mime.ParseMediaType(ct)
	page.ContentType = media
	if !textual(media) {
		return page, refuse(CodeUnsupportedContent, "%s is not a text type", orUnknown(media))
	}
	if page.Truncated {
		body = dropPartialRune(body)
	}
	text := string(bytes.ToValidUTF8(body, []byte("�")))
	if media == "text/html" || media == "application/xhtml+xml" {
		page.Title, page.Text = HTMLText(text, resp.Request.URL)
	} else {
		page.Text = text
	}
	return page, nil
}

func (f *Fetcher) failure(ctx context.Context, err error) error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, errBlocked):
		return refuse(CodeBlockedAddress, "the host resolves to an address that is not public")
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return refuse(CodeTimeout, "no complete answer within %s", f.timeout())
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return refuse(CodeFetchFailed, "the host name did not resolve")
	}
	return refuse(CodeFetchFailed, "%s", fetchReason(err))
}

// fetchReason is err without the URL, which the caller already has.
func fetchReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Sprint(err)
}

func textual(media string) bool {
	switch {
	case strings.HasPrefix(media, "text/"):
		return true
	case media == "application/json", media == "application/xml", media == "application/xhtml+xml",
		strings.HasPrefix(media, "application/") && (strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml")):
		return true
	}
	return false
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown type"
	}
	return s
}

// Slice returns at most n bytes of text from offset, cut at a line break when one falls in the
// second half of the window and never inside a UTF-8 sequence, and the offset of the rest, or -1
// when nothing is left.
func Slice(text string, offset, n int) (string, int) {
	if offset < 0 || offset >= len(text) {
		return "", -1
	}
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset++
	}
	rest := text[offset:]
	if len(rest) <= n {
		return rest, -1
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(rest[cut]) {
		cut--
	}
	if i := strings.LastIndexByte(rest[:cut], '\n'); i >= n/2 {
		cut = i + 1
	}
	return rest[:cut], offset + cut
}

// dropPartialRune drops a UTF-8 sequence cut short at the end of b.
func dropPartialRune(b []byte) []byte {
	for k := 1; k <= utf8.UTFMax && k <= len(b); k++ {
		if utf8.RuneStart(b[len(b)-k]) {
			if !utf8.FullRune(b[len(b)-k:]) {
				return b[:len(b)-k]
			}
			return b
		}
	}
	return b
}
