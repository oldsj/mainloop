package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/privileged"
	"github.com/oldsj/mainloop/server/internal/privileged/privilegedtest"
)

const (
	appNS       = "widgets"
	secretValue = "fictional-s3cret-value"
)

type execEnv struct {
	*env
	op   *privilegedtest.OnePassword
	kube *fake.Clientset
	ex   *privileged.Runner
}

// newExecEnv is serve-api (with an executor deployed) and serve-executor over one database.
func newExecEnv(t *testing.T) *execEnv {
	t.Helper()
	e := &execEnv{env: newEnv(t), op: privilegedtest.NewOnePassword(), kube: fake.NewClientset()}
	e.svc.ExecutorOn = true
	e.op.Put("Apps", "widgets-db", "password", secretValue)
	e.ex = e.executor()
	return e
}

func (e *execEnv) executor() *privileged.Runner {
	return &privileged.Runner{Service: &privileged.Service{
		Pool: e.db.Pool, OwnerID: owner, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Executor: true,
		OnePassword: e.op, Secrets: privileged.KubeSecrets{Client: e.kube}, SecretNamespaces: []string{appNS},
	}}
}

func (e *execEnv) runExecutor(t *testing.T) {
	t.Helper()
	if err := e.ex.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func copyArgs(ns, secret, key string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"vault": "Apps", "item": "widgets-db", "field": "password", "namespace": ns, "secret": secret, "key": key})
	return b
}

func (e *execEnv) requestCopy(t *testing.T, requestID string, args json.RawMessage) privileged.Action {
	t.Helper()
	a, err := e.svc.Request(context.Background(), mainSession, privileged.TypeItemToSecret, args, "The widgets app needs its database password.", requestID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// approve answers the action's card with Approve and its digest, and lets serve-api record it.
func (e *execEnv) approve(t *testing.T, id string) privileged.Action {
	t.Helper()
	a := e.get(t, id)
	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
	e.run(t)
	if a = e.get(t, id); a.State != privileged.StateApproved {
		t.Fatalf("after approval: %+v", a)
	}
	return a
}

func (e *execEnv) secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	s, err := e.kube.CoreV1().Secrets(appNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Given an executor type, when main requests it, then serve-api only records the request;
// serve-executor preflights it without opening a card; serve-api opens the card; and after an
// Approve with the digest, serve-api does not run it but serve-executor does, creating the Secret.
// No card, event, result or message ever carries the value.
func TestItemToSecretRunsInTheExecutorOnly(t *testing.T) {
	e := newExecEnv(t)
	a := e.requestCopy(t, "copy-1", copyArgs(appNS, "widgets-db", "password"))
	if a.State != privileged.StateRequested || a.Digest != "" {
		t.Fatalf("request = %+v", a)
	}
	e.run(t) // serve-api: not its type to preflight
	if got := e.get(t, a.ID); got.State != privileged.StateRequested || len(e.openItems(t)) != 0 {
		t.Fatalf("serve-api touched it: %+v", got)
	}

	e.runExecutor(t)
	a = e.get(t, a.ID)
	if a.State != privileged.StatePendingApproval || len(a.Digest) != 64 || len(e.openItems(t)) != 0 {
		t.Fatalf("after executor preflight: %+v, items %d", a, len(e.openItems(t)))
	}
	e.run(t) // serve-api opens the card
	items := e.openItems(t)
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	content := items[0].QueueItem.Content
	for _, want := range []string{`Copy 1Password "widgets-db › password" to Secret widgets/widgets-db`, `vault "Apps"`, "version 1",
		"does not exist; Mainloop creates it", "never reaches an agent", "its claim"} {
		if !strings.Contains(items[0].Title+"\n"+content, want) {
			t.Errorf("card lacks %q:\n%s\n%s", want, items[0].Title, content)
		}
	}

	e.approve(t, a.ID)
	e.run(t) // serve-api does not claim an executor type
	if got := e.get(t, a.ID); got.State != privileged.StateApproved || e.op.Reads != 0 {
		t.Fatalf("serve-api ran it: %+v", got)
	}
	e.runExecutor(t)
	a = e.get(t, a.ID)
	if a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	s := e.secret(t, "widgets-db")
	if string(s.Data["password"]) != secretValue || s.Annotations[privileged.AnnotationAction] != a.ID || s.Type != corev1.SecretTypeOpaque {
		t.Fatalf("secret = %+v", s)
	}
	var res privileged.ItemToSecretResult
	if err := json.Unmarshal(a.Result, &res); err != nil || !res.Created || res.Key != "password" {
		t.Fatalf("result = %s", a.Result)
	}

	e.run(t) // delivery
	if len(e.sender.msgs) != 1 || !strings.Contains(e.sender.msgs[0].text, "ended: succeeded") {
		t.Fatalf("messages = %+v", e.sender.msgs)
	}
	_, evs, err := e.svc.Get(context.Background(), mainSession.SessionID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	all := string(a.Result) + string(a.Facts) + content + e.sender.msgs[0].text
	for _, ev := range evs {
		all += ev.Detail
	}
	if strings.Contains(all, secretValue) {
		t.Fatal("the value leaked into the record, the card or the message")
	}
}

// Given a Secret with other keys, when the copy runs, then only its key changes, the card says it
// overwrites, and the other keys stay.
func TestItemToSecretPatchesOneKeyOfAnExistingSecret(t *testing.T) {
	e := newExecEnv(t)
	if _, err := e.kube.CoreV1().Secrets(appNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets-db", Namespace: appNS, ResourceVersion: "7"},
		Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"password": []byte("old"), "username": []byte("widgets")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := e.requestCopy(t, "copy-1", copyArgs(appNS, "widgets-db", "password"))
	e.runExecutor(t)
	e.run(t)
	items := e.openItems(t)
	if len(items) != 1 || !strings.Contains(items[0].QueueItem.Content, "keys: password, username") ||
		!strings.Contains(items[0].QueueItem.Content, "overwrites an existing key") {
		t.Fatalf("card = %+v", items)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	s := e.secret(t, "widgets-db")
	if string(s.Data["password"]) != secretValue || string(s.Data["username"]) != "widgets" {
		t.Fatalf("data = %v", s.Data)
	}
}

// Given targets the executor must not write, when they are requested, then the request or its
// preflight refuses them and no card opens.
func TestItemToSecretRefusals(t *testing.T) {
	e := newExecEnv(t)
	ctx := context.Background()
	for _, ns := range []string{"kube-system", "mainloop", "onepassword", "argocd", "default"} {
		if _, err := e.svc.Request(ctx, mainSession, privileged.TypeItemToSecret, copyArgs(ns, "x", "k"), "r", "deny-"+ns); !errors.Is(err, privileged.ErrInvalid) {
			t.Errorf("namespace %s: err = %v", ns, err)
		}
	}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "argo", Namespace: appNS, Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "app:/Secret:widgets/argo"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: appNS, OwnerReferences: []metav1.OwnerReference{{Kind: "OnePasswordItem", Name: "owned", Controller: ptr(true)}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: appNS}, Type: corev1.SecretTypeTLS},
		{ObjectMeta: metav1.ObjectMeta{Name: "frozen", Namespace: appNS}, Immutable: ptr(true)},
	} {
		if _, err := e.kube.CoreV1().Secrets(appNS).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.op.Put("Apps", "blank", "password", "")
	cases := map[string]json.RawMessage{
		"not allowed":   copyArgs("other-app", "x", "k"),
		"argo":          copyArgs(appNS, "argo", "k"),
		"owned":         copyArgs(appNS, "owned", "k"),
		"tls":           copyArgs(appNS, "tls", "k"),
		"immutable":     copyArgs(appNS, "frozen", "k"),
		"no such item":  json.RawMessage(`{"vault":"Apps","item":"nope","field":"password","namespace":"widgets","secret":"x","key":"k"}`),
		"empty field":   json.RawMessage(`{"vault":"Apps","item":"blank","field":"password","namespace":"widgets","secret":"x","key":"k"}`),
		"no such field": json.RawMessage(`{"vault":"Apps","item":"widgets-db","field":"pin","namespace":"widgets","secret":"x","key":"k"}`),
	}
	ids := map[string]string{}
	for name, args := range cases {
		ids[name] = e.requestCopy(t, name, args).ID
		e.runExecutor(t) // each ends at preflight, so none counts against the pending limit
	}
	e.run(t)
	for name, id := range ids {
		if a := e.get(t, id); a.State != privileged.StatePreflightFailed {
			t.Errorf("%s: %+v", name, a)
		}
	}
	if n := len(e.openItems(t)); n != 0 {
		t.Fatalf("cards = %d", n)
	}
	if len(e.sender.msgs) != len(cases) {
		t.Fatalf("messages = %d, want one per refusal", len(e.sender.msgs))
	}
}

// Given an approved copy, when the item or the Secret changed after the owner saw it, then nothing
// is written and the action is stale.
func TestItemToSecretStaleTargets(t *testing.T) {
	for name, change := range map[string]func(e *execEnv){
		"item changed":   func(e *execEnv) { e.op.Put("Apps", "widgets-db", "password", "rotated") },
		"secret created": func(e *execEnv) { mustCreate(e, "widgets-db") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newExecEnv(t)
			a := e.requestCopy(t, "copy-1", copyArgs(appNS, "widgets-db", "password"))
			e.runExecutor(t)
			e.run(t)
			e.approve(t, a.ID)
			change(e)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != privileged.StateStale {
				t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
			}
			if s, err := e.kube.CoreV1().Secrets(appNS).Get(context.Background(), "widgets-db", metav1.GetOptions{}); err == nil && string(s.Data["password"]) == secretValue {
				t.Fatal("the value was written")
			}
		})
	}
}

// Given an attempt whose write landed but whose answer was lost, when the next attempt runs, then
// it observes the Secret carries this action and succeeds without writing again.
func TestItemToSecretLostAnswerIsObserved(t *testing.T) {
	e := newExecEnv(t)
	a := e.requestCopy(t, "copy-1", copyArgs(appNS, "widgets-db", "password"))
	e.runExecutor(t)
	e.run(t)
	e.approve(t, a.ID)
	lost := &lostSecrets{Secrets: privileged.KubeSecrets{Client: e.kube}}
	e.ex.Secrets = lost
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateExecuting || lost.writes != 1 {
		t.Fatalf("after the lost answer: %+v, writes %d", a, lost.writes)
	}
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || lost.writes != 1 {
		t.Fatalf("after observing: %+v, writes %d", a, lost.writes)
	}
	var res privileged.ItemToSecretResult
	if err := json.Unmarshal(a.Result, &res); err != nil || !res.Observed {
		t.Fatalf("result = %s", a.Result)
	}
}

// Given no executor deployed, when an executor type is requested, then it is unsupported; and
// given one deployed that never picks a request up, then serve-api ends it after the pickup window
// and tells the agent.
func TestItemToSecretNeedsALiveExecutor(t *testing.T) {
	e := newExecEnv(t)
	e.svc.ExecutorOn = false
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypeItemToSecret, copyArgs(appNS, "x", "k"), "r", "r1"); !errors.Is(err, privileged.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	e.svc.ExecutorOn = true
	a := e.requestCopy(t, "r2", copyArgs(appNS, "x", "k"))
	e.exec(t, `UPDATE privileged_actions SET created_at = now() - interval '11 minutes' WHERE id = $1`, a.ID)
	e.run(t)
	a = e.get(t, a.ID)
	if a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), "executor did not preflight") {
		t.Fatalf("action = %+v", a)
	}
	e.run(t)
	if len(e.sender.msgs) != 1 {
		t.Fatalf("messages = %+v", e.sender.msgs)
	}
}

// Given the executor's own database role with only the grants migration 00890 gives, when the
// executor preflights and runs an action under it, then it works; and the role can read no other
// table and cannot record a decision.
func TestExecutorRunsUnderItsOwnRole(t *testing.T) {
	e := newExecEnv(t)
	ctx := context.Background()
	e.exec(t, `DO $$ BEGIN CREATE ROLE mainloop_executor LOGIN PASSWORD 'fictional-executor-pw';
		EXCEPTION WHEN duplicate_object THEN NULL; END $$`)
	var granted bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT privileged_executor_grants()`).Scan(&granted); err != nil || !granted {
		t.Fatalf("grants: %v %v", granted, err)
	}
	u, err := url.Parse(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("mainloop_executor", "fictional-executor-pw")
	pool, err := db.NewPoolFromURL(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ex := e.executor()
	ex.Pool = pool

	a := e.requestCopy(t, "copy-1", copyArgs(appNS, "widgets-db", "password"))
	if err := ex.Run(ctx); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	e.approve(t, a.ID)
	if err := ex.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	for _, sql := range []string{
		`SELECT count(*) FROM attention_items`,
		`UPDATE privileged_actions SET decided_at = now(), approved_digest = digest`,
		`UPDATE privileged_actions SET args = '{}'`,
		`DELETE FROM privileged_action_events`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: err = %v, want permission denied", sql, err)
		}
	}
}

type lostSecrets struct {
	privileged.Secrets
	writes int
}

func (l *lostSecrets) Create(ctx context.Context, ns, name, key string, value []byte, id string) (string, error) {
	l.writes++
	if _, err := l.Secrets.Create(ctx, ns, name, key, value, id); err != nil {
		return "", err
	}
	return "", errors.New("connection reset by peer")
}

func mustCreate(e *execEnv, name string) {
	_, _ = e.kube.CoreV1().Secrets(appNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}, Type: corev1.SecretTypeOpaque,
	}, metav1.CreateOptions{})
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
