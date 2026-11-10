// logproxy runs mockllm on 127.0.0.1:8081 and serves it on :8080. For every request mockllm
// does not answer with 200, it logs the method, path, status, message roles and last message,
// so a fixture gap shows what the client actually sent.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
)

const maxLogged = 8 << 10

func main() {
	mock := exec.Command("/mockllm", "-config", "/fixture.json", "-listen-addr", "127.0.0.1:8081")
	mock.Stdout, mock.Stderr = os.Stdout, os.Stderr
	if err := mock.Start(); err != nil {
		log.Fatalf("start mockllm: %v", err)
	}
	go func() { log.Fatalf("mockllm exited: %v", mock.Wait()) }()

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "127.0.0.1:8081"})
	log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		proxy.ServeHTTP(rec, r)
		if rec.status != http.StatusOK {
			log.Printf("%s %s -> %d %s", r.Method, r.URL.RequestURI(), rec.status, summarize(body))
		}
	})))
}

// summarize returns the roles of all messages and the last message, truncated.
func summarize(body []byte) string {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return truncate(body)
	}
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		var role struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(m, &role)
		roles = append(roles, role.Role)
	}
	return "roles=" + truncate(must(json.Marshal(roles))) + " last=" + truncate(req.Messages[len(req.Messages)-1])
}

func must(b []byte, _ error) []byte { return b }

func truncate(b []byte) string {
	if len(b) > maxLogged {
		return string(b[:maxLogged]) + "...(truncated)"
	}
	return string(b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush keeps streamed responses flowing through the proxy.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
