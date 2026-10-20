package pushgate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	ka "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

type admissionBody struct {
	data    *bytes.Reader
	reads   atomic.Int64
	entered chan struct{}
	release <-chan struct{}
}

func (b *admissionBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.data.Len() > 0 {
		return b.data.Read(p)
	}
	close(b.entered)
	<-b.release
	return 0, io.EOF
}

func blockedUpload(s *Server, token string, release <-chan struct{}) (*admissionBody, <-chan *httptest.ResponseRecorder) {
	b := &admissionBody{data: bytes.NewReader(bytes.Repeat([]byte{'x'}, 32<<10)), entered: make(chan struct{}), release: release}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		r.Body = io.NopCloser(b)
		r.ContentLength = -1
		r.TransferEncoding = []string{"chunked"}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		done <- w
	}()
	return b, done
}

func uploadResult(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not complete")
		return nil
	}
}

func uploadEntered(t *testing.T, b *admissionBody) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("admitted upload did not spool")
	}
}

func admissionRefused(t *testing.T, w *httptest.ResponseRecorder, b *admissionBody) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable || !strings.HasPrefix(w.Body.String(), "admission_busy ") || w.Header().Get("Retry-After") != "1" || b.reads.Load() != 0 {
		t.Fatalf("admission refusal: status=%d body=%s reads=%d", w.Code, w.Body.String(), b.reads.Load())
	}
}

func admissionClean(t *testing.T, e *env) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(e.server.SpoolRoot, "receive-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("retained scratch: %v %v", files, err)
	}
	var claims int
	if err = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM git_push_admissions`).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("retained admissions: %d %v", claims, err)
	}
	receiveScratch.Lock()
	used := receiveScratch.used
	receiveScratch.Unlock()
	if used != 0 {
		t.Fatalf("retained byte reservations: %d", used)
	}
}

func TestReviewProbeConcurrentSameBranchSpooling(t *testing.T) {
	e := newEnv(t)
	e.server.Limits.BodyBytes = 64 << 10
	e.server.Limits.DiskBytes = 128 << 10 // spare capacity isolates the database branch claim
	release := make(chan struct{})
	first, done := blockedUpload(e.server, e.token, release)
	defer func() { close(release); uploadResult(t, done); admissionClean(t, e) }()
	uploadEntered(t, first)
	// Separate listener and service objects share only the database and the pod budget.
	other := *e.server
	service := *e.svc
	other.Service = &service
	for range 2 {
		b, refused := blockedUpload(&other, e.token, release)
		admissionRefused(t, uploadResult(t, refused), b)
	}
	files, err := filepath.Glob(filepath.Join(e.server.SpoolRoot, "receive-*", "request"))
	if err != nil || len(files) != 1 || e.publicationCount() != 0 {
		t.Fatalf("same-branch spool admission: files=%v publications=%d err=%v", files, e.publicationCount(), err)
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Size() != 32<<10 {
		t.Fatalf("first body did not spool: %v %v", info, err)
	}
	if _, err = e.pool.Exec(context.Background(), `UPDATE workspace_writer_claims SET held=false`); err == nil {
		t.Fatal("spooling admission allowed writer release")
	}
}

func additionalBranch(t *testing.T, e *env, id, branch string) (*Server, string) {
	t.Helper()
	e.exec(`INSERT INTO sessions (id,user_id,project_id,title,description,prompt,status,repo_url,branch_name)
		VALUES ($1,'owner','p','t','d','p','active','https://github.com/oldsj/alpha',$2)`, id, branch)
	e.exec(`INSERT INTO agent_bindings (session_id,kind,role,mcp_grant_kind,kagent_session_id)
		VALUES ($1,'claude','agent','workspace',$1)`, id)
	e.exec(`INSERT INTO workspaces (id,user_id,project_id,repo_url,ref,branch,agent_kind,agent_namespace,agent_name,request_id,environment)
		SELECT $1,user_id,project_id,repo_url,ref,$2,agent_kind,agent_namespace,agent_name,$1,environment FROM workspaces WHERE id='ws'`, id, branch)
	e.exec(`INSERT INTO workspace_writer_claims (owner_id,repository,branch,generation,binding_id) VALUES ('owner','oldsj/alpha',$2,1,$1)`, id, branch)
	if _, err := (&gittransport.Enroller{Pool: e.pool, Creds: e.creds}).Enroll(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	n := proto.Clone(e.native.session).(*ka.Session)
	n.Id, n.ContextId, n.Workspace.Branch = id, id, branch
	svc := &Service{Pool: e.pool, Native: &nativeFake{session: n}, Upstream: e.upstream, Credentials: e.creds}
	issued, err := svc.Enroll(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s := *e.server
	s.Service = svc
	return &s, agentauth.Token(e.creds.Key, issued.CredentialID)
}

func TestDifferentBranchesExhaustProcessScratchBudget(t *testing.T) {
	e := newEnv(t)
	e.server.Limits.BodyBytes = 32 << 10
	e.server.Limits.DiskBytes = 64 << 10
	second, token2 := additionalBranch(t, e, "ws2", "feature/second")
	third, token3 := additionalBranch(t, e, "ws3", "feature/third")
	release := make(chan struct{})
	b1, done1 := blockedUpload(e.server, e.token, release)
	b2, done2 := blockedUpload(second, token2, release)
	completed := false
	defer func() {
		if !completed {
			close(release)
			uploadResult(t, done1)
			uploadResult(t, done2)
		}
		admissionClean(t, e)
	}()
	uploadEntered(t, b1)
	uploadEntered(t, b2)
	b3, refused := blockedUpload(third, token3, release)
	admissionRefused(t, uploadResult(t, refused), b3)
	files, err := filepath.Glob(filepath.Join(e.server.SpoolRoot, "receive-*", "request"))
	if err != nil || len(files) != 2 {
		t.Fatalf("budget allowed another spool: %v %v", files, err)
	}
	close(release)
	uploadResult(t, done1)
	uploadResult(t, done2)
	completed = true
	admissionClean(t, e)
	// Both invalid bodies released their claims and reservations. The refused third branch
	// can now retry without a publication, body spool, or upstream traffic.
	before, pushes := e.upstream.counts()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", strings.NewReader("0000"))
	r.Header.Set("Authorization", "Bearer "+token3)
	r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	w := httptest.NewRecorder()
	third.Handler().ServeHTTP(w, r)
	after, pushesAfter := e.upstream.counts()
	if w.Code != http.StatusOK || w.Body.Len() != 0 || e.publicationCount() != 0 || before != after || pushes != pushesAfter {
		t.Fatalf("budget retry probe: %d %s", w.Code, w.Body.String())
	}
}

func TestExpiredAdmissionCannotPublishOrReleaseSuccessor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, err := e.svc.authenticate(ctx, "Bearer "+e.token)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.check(ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := e.svc.admit(ctx, c, a, "old-handler", e.server.Limits.defaults())
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE git_push_admissions SET lease_until=now()-interval '1 second'`)
	cmd, err := receiveCommands(bytes.NewReader(e.body(zeroOID, e.head, "refs/heads/feature/test")), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.reserve(ctx, c, a, cmd, &spool{}, "old-handler"); !errors.Is(err, denial("admission_busy")) || e.publicationCount() != 0 {
		t.Fatalf("expired admission reserved intent: %v", err)
	}
	current, err := e.svc.admit(ctx, c, a, "new-handler", e.server.Limits.defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer e.svc.releaseAdmission(ctx, current)
	e.svc.releaseAdmission(ctx, old)
	if _, err = dbq.New(e.pool).LockPushAdmission(ctx, current); err != nil {
		t.Fatal("old cleanup released successor")
	}
}

func TestReviewProbeNonCommitRefusal(t *testing.T) {
	e := newEnv(t)
	blob := e.git(e.work, "rev-parse", "HEAD:file")
	w := e.request(e.body(zeroOID, blob, "refs/heads/feature/test"), nil)
	_, pushes := e.upstream.counts()
	if w.Code != http.StatusForbidden || !strings.HasPrefix(w.Body.String(), "commit_required ") || pushes != 0 || e.state() != dbstate.PublicationRejected {
		t.Fatalf("non-commit classification: %d %s", w.Code, w.Body.String())
	}
	admissionClean(t, e)
}

func TestExpiredAdmissionBeforeDispatch(t *testing.T) {
	e := newEnv(t)
	e.upstream.before = func(stage string) {
		if stage == "metadata" {
			e.exec(`UPDATE git_push_admissions SET lease_until=now()-interval '1 second'`)
		}
	}
	body := e.body(zeroOID, e.head, "refs/heads/feature/test")
	w := e.request(body, nil)
	_, pushes := e.upstream.counts()
	if w.Code != http.StatusServiceUnavailable || !strings.HasPrefix(w.Body.String(), "admission_busy ") || pushes != 0 || e.state() != dbstate.PublicationRejected {
		t.Fatalf("expired admission dispatched: %d %s", w.Code, w.Body.String())
	}
	admissionClean(t, e)
	e.upstream.before = nil
	if retry := e.request(body, nil); retry.Code != http.StatusOK {
		t.Fatalf("expired admission retained fence: %d %s", retry.Code, retry.Body.String())
	}
}

type reviewCountingReader struct {
	r    io.Reader
	read int64
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }

func TestAdmissionCleanupOnBodyFailure(t *testing.T) {
	for _, kind := range []string{"disconnect", "cancel", "panic"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := e.request(nil, func(r *http.Request) {
				*r = *r.WithContext(ctx)
				r.ContentLength = -1
				r.Body = io.NopCloser(io.MultiReader(strings.NewReader("xxxxx"), readerFunc(func([]byte) (int, error) {
					switch kind {
					case "cancel":
						cancel()
					case "panic":
						panic("fixture body failure")
					}
					return 0, io.ErrUnexpectedEOF
				})))
			})
			if w.Code == http.StatusOK || e.publicationCount() != 0 {
				t.Fatal("failed body admitted a publication")
			}
			admissionClean(t, e)
			if retry := e.request([]byte("0000"), nil); retry.Code != http.StatusOK {
				t.Fatalf("failure retained admission: %d %s", retry.Code, retry.Body.String())
			}
		})
	}
}

func (r *reviewCountingReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.read += int64(n)
	return n, err
}

func TestReviewProbeDiskBudget(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(cache, "mainloop-disk-budget-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	limits := (Limits{BodyBytes: 1 << 20, DiskBytes: 64 << 10}).defaults()
	source := &reviewCountingReader{r: bytes.NewReader(make([]byte, 2<<20))}
	sp, err := newSpool(context.Background(), root, source, limits)
	if sp != nil {
		sp.close()
	}
	if !errors.Is(err, denial("body_limit")) || source.read > limits.DiskBytes+1 {
		t.Fatalf("disk-budget refusal: err=%v read=%d budget=%d", err, source.read, limits.DiskBytes)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatal("rejected body retained scratch")
	}
}

func TestScratchWriterPreservesFreeReserveDuringCopy(t *testing.T) {
	var out bytes.Buffer
	free := freeReserve + 4
	w := &scratchWriter{ctx: context.Background(), w: &out, available: func() (uint64, error) { return free, nil }}
	if n, err := w.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("available allocation: %d %v", n, err)
	}
	free = freeReserve
	if n, err := w.Write([]byte("5")); n != 0 || !errors.Is(err, denial("disk_unavailable")) || out.String() != "1234" {
		t.Fatalf("free-space reserve consumed: %d %v", n, err)
	}
}

// The production spool volume is an emptyDir with fsGroup: its root is group- and
// world-writable, so the gate must spool into a private directory it creates inside it.
func TestSpoolInsideASharedVolumeRoot(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	mount, err := os.MkdirTemp(cache, "mainloop-spool-volume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(mount) })
	if err = os.Chmod(mount, 0o2777); err != nil {
		t.Fatal(err)
	}
	limits := (Limits{BodyBytes: 1 << 20, DiskBytes: 1 << 20}).defaults()
	if sp, err := newSpool(context.Background(), mount, bytes.NewReader([]byte("0000")), limits); sp != nil || !errors.Is(err, denial("private_spool_required")) {
		t.Fatalf("shared volume root: err=%v", err)
	}
	sp, err := newSpool(context.Background(), filepath.Join(mount, filepath.Base(defaultSpoolRoot)), bytes.NewReader([]byte("0000")), limits)
	if err != nil {
		t.Fatalf("private directory inside the volume: %v", err)
	}
	sp.close()
	if filepath.Dir(defaultSpoolRoot) != "/var/lib/mainloop/git" {
		t.Fatalf("default spool root %q is not inside the spool volume", defaultSpoolRoot)
	}
}
