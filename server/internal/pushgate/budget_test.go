package pushgate

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The review fixture used eight bodies whose combined validated scratch was 298,932 bytes in a
// 256 KiB pod budget. Retain the same real packs, but expect capacity refusal once validation has
// reserved seed/Git space. Releasing that reservation permits every branch to retry successfully.
func TestReReviewProbeQuarantineBudgetAccounting(t *testing.T) {
	e := newEnv(t)
	e.server.Limits.BodyBytes, e.server.Limits.DiskBytes = 32<<10, 256<<10
	fixture := make([]byte, 16<<10)
	if _, err := rand.Read(fixture); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.work, "incompressible-fixture"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	e.git(e.work, "add", "incompressible-fixture")
	e.git(e.work, "commit", "-m", "bounded resource fixture")
	e.head = e.git(e.work, "rev-parse", "HEAD")
	servers, tokens, branches := []*Server{e.server}, []string{e.token}, []string{"feature/test"}
	for i := 1; i < 8; i++ {
		branch := fmt.Sprintf("feature/budget-%d", i)
		s, token := additionalBranch(t, e, fmt.Sprintf("budget-ws-%d", i), branch)
		servers, tokens, branches = append(servers, s), append(tokens, token), append(branches, branch)
	}
	bodies := make([][]byte, len(servers))
	for i, branch := range branches {
		bodies[i] = e.body(zeroOID, e.head, "refs/heads/"+branch)
	}
	validated, release := make(chan struct{}), make(chan struct{})
	e.upstream.before = func(stage string) {
		if stage == "metadata" {
			close(validated)
			<-release
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.request(bodies[0], nil) }()
	completed := false
	defer func() {
		if !completed {
			close(release)
			uploadResult(t, done)
		}
		admissionClean(t, e)
	}()
	select {
	case <-validated:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded push did not reach post-validation barrier")
	}
	var total int64
	if err := filepath.WalkDir(e.server.SpoolRoot, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	receiveScratch.Lock()
	reserved := receiveScratch.used
	receiveScratch.Unlock()
	if total > reserved || reserved > e.server.Limits.DiskBytes || total <= e.server.Limits.BodyBytes {
		t.Fatalf("unaccounted validation scratch: actual=%d reserved=%d budget=%d", total, reserved, e.server.Limits.DiskBytes)
	}
	t.Logf("body limit=%d actual quarantine=%d reserved=%d shared budget=%d", e.server.Limits.BodyBytes, total, reserved, e.server.Limits.DiskBytes)
	for i := 1; i < len(servers); i++ {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", strings.NewReader(string(bodies[i])))
		r.Header.Set("Authorization", "Bearer "+tokens[i])
		r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		w := httptest.NewRecorder()
		servers[i].Handler().ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || !strings.HasPrefix(w.Body.String(), "admission_busy ") {
			t.Fatal("validation admitted another branch beyond its shared budget")
		}
	}
	close(release)
	w := uploadResult(t, done)
	completed = true
	if w.Code != http.StatusOK {
		t.Fatalf("bounded fixture refused: %d", w.Code)
	}
	e.upstream.before = nil
	admissionClean(t, e)
	for i := 1; i < len(servers); i++ {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", strings.NewReader(string(bodies[i])))
		r.Header.Set("Authorization", "Bearer "+tokens[i])
		r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		w := httptest.NewRecorder()
		servers[i].Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("branch %d did not recover after validation cleanup: %d", i, w.Code)
		}
	}
}
