package httpx

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// compressMinBytes is the smallest JSON body worth compressing: below it the gzip framing and the
// CPU cost buy nothing, and most answers (an error, a health probe, one small row) stay as written.
const compressMinBytes = 1024

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// Compress gzips a JSON response of at least compressMinBytes for a caller that accepts gzip. The
// owner's task list is over a megabyte and the app re-reads it while it is on screen, so this is
// most of what a phone downloads.
//
// Everything else passes through untouched: a body of another type (the event streams are
// text/event-stream), one already encoded, a response with no body, a small one, an upgrade (the
// live relay's WebSocket), and a handler that flushes before the threshold, which is streaming.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		// The answer depends on the header whether or not this one is compressed.
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		// Not deferred: after a panic nothing held back is sent, so the recovery middleware can
		// still write its own answer.
		cw.finish()
	})
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip: named with a non-zero weight.
func acceptsGzip(header string) bool {
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q, found := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !found {
			return true
		}
		weight, err := strconv.ParseFloat(q, 64)
		return err == nil && weight > 0
	}
	return false
}

type compressMode int

const (
	// compressPending holds the status and the first bytes until the body reaches the threshold
	// or the handler returns.
	compressPending compressMode = iota
	compressPlain
	compressGzip
)

// compressWriter decides once per response, at the threshold, whether the body is gzipped. Until
// then it holds the status and the bytes written so far, so a small body is sent as written with
// its own Content-Length.
type compressWriter struct {
	http.ResponseWriter
	mode   compressMode
	status int
	held   []byte
	gz     *gzip.Writer
}

func (w *compressWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	if !w.compressible() {
		w.plain()
	}
}

// compressible is asked when the handler has chosen its status and headers.
func (w *compressWriter) compressible() bool {
	if w.status < http.StatusOK || w.status == http.StatusNoContent || w.status == http.StatusNotModified {
		return false
	}
	h := w.Header()
	if h.Get("Content-Encoding") != "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

func (w *compressWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	switch w.mode {
	case compressPlain:
		return w.ResponseWriter.Write(b)
	case compressGzip:
		return w.gz.Write(b)
	case compressPending:
	}
	w.held = append(w.held, b...)
	if len(w.held) < compressMinBytes {
		return len(b), nil
	}
	if err := w.startGzip(); err != nil {
		return 0, err
	}
	return len(b), nil
}

// plain sends the status and whatever was held, uncompressed.
func (w *compressWriter) plain() {
	w.mode = compressPlain
	w.ResponseWriter.WriteHeader(w.status)
	if len(w.held) > 0 {
		// A failed write is a broken connection; the handler's next write reports it.
		_, _ = w.ResponseWriter.Write(w.held)
		w.held = nil
	}
}

func (w *compressWriter) startGzip() error {
	w.mode = compressGzip
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	w.ResponseWriter.WriteHeader(w.status)
	gz, _ := gzipWriters.Get().(*gzip.Writer)
	gz.Reset(w.ResponseWriter)
	w.gz = gz
	held := w.held
	w.held = nil
	_, err := gz.Write(held)
	return err
}

// Flush sends what the handler has written so far. A handler that flushes below the threshold is
// streaming, so its response goes out as written from here on.
func (w *compressWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.mode == compressPending {
		w.plain()
	}
	if w.mode == compressGzip {
		_ = w.gz.Flush()
	}
	// Flush errors are a broken client connection; the next write reports them.
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// finish ends the response once the handler has returned: a body that stayed under the threshold
// goes out as written, and a gzip stream gets its trailer.
func (w *compressWriter) finish() {
	switch w.mode {
	case compressPending:
		// A handler that wrote nothing leaves the status to the server, as without this wrapper.
		if w.status != 0 {
			w.plain()
		}
	case compressGzip:
		_ = w.gz.Close()
		w.gz.Reset(nil)
		gzipWriters.Put(w.gz)
		w.gz = nil
	case compressPlain:
	}
}

func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
