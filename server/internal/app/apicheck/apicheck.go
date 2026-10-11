// Package apicheck validates every response a test produces against api/openapi.yaml.
//
// Wrap puts a recorder in front of a handler. After each request it checks the status, headers
// and JSON body against the operation's declared responses, so an undeclared status, a body of
// the wrong shape, or a list past its declared maxItems fails the test that produced it. Requests
// the contract does not route (an unknown path or method) have no declared response and are not
// checked. Event streams are checked for status and content type only, since the stream has no
// JSON body to validate.
package apicheck

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/oldsj/mainloop/server/internal/api"
)

var (
	once   sync.Once
	router routers.Router
	loaded error
)

func contractRouter() (routers.Router, error) {
	once.Do(func() {
		spec, err := api.GetSpec()
		if err != nil {
			loaded = err
			return
		}
		// A spec with servers makes the router match on Host; the tests choose their own.
		spec.Servers = nil
		router, loaded = gorillamux.NewRouter(spec)
	})
	return router, loaded
}

// Wrap returns h with every response checked against the contract. Violations fail t.
func Wrap(t testing.TB, h http.Handler) http.Handler {
	t.Helper()
	rt, err := contractRouter()
	if err != nil {
		t.Fatalf("apicheck: load contract: %v", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		check(t, rt, r, rec)
	})
}

// recorder passes writes through and keeps a copy of the status, headers and body.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.body.Write(b[:n])
	return n, err
}

func (r *recorder) Flush() { _ = http.NewResponseController(r.ResponseWriter).Flush() }

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func check(t testing.TB, rt routers.Router, req *http.Request, rec *recorder) {
	t.Helper()
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	// The request body was consumed by the handler; routing needs only the method and URL.
	probe := req.Clone(context.Background())
	probe.Body = http.NoBody
	route, params, err := rt.FindRoute(probe)
	if err != nil {
		return
	}
	header := rec.Header().Clone()
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: probe, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status:  status,
		Header:  header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	if strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
		in.Options.ExcludeResponseBody = true
	} else {
		in.SetBodyBytes(rec.body.Bytes())
	}
	if err := openapi3filter.ValidateResponse(req.Context(), in); err != nil {
		t.Errorf("response breaks the contract: %s %s -> %d: %v\nbody: %.300s", req.Method, req.URL.Path, status, err, rec.body.String())
	}
}
