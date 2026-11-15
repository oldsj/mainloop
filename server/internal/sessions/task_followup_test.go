package sessions_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// A task's kagent Session carries the task's context. When kagent no longer has it, the next
// message fails with that said, and no new, empty Session is made for it.
func TestATaskWhoseSessionIsGoneIsNotGivenANewOne(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	e.run()
	sid := e.taskSession()
	old := e.bindingKsid(sid)
	if _, err := e.other().DeleteSession(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	creates := len(e.srv.CreateRequests())

	d := e.waitState(e.sendTo(sid, "and the staging cluster?"), dbstate.DeliveryFailed)
	if d.Detail == nil || !strings.Contains(*d.Detail, "context is lost; retry the task") {
		t.Fatalf("delivery detail = %v", d.Detail)
	}
	if got := e.bindingKsid(sid); got != old || len(e.srv.CreateRequests()) != creates {
		t.Fatalf("the task got a new Session: %q (was %q), creates %d (was %d)", got, old, len(e.srv.CreateRequests()), creates)
	}
}

// A finished task's credential was revoked; a follow-up that reopens it gets the credential back,
// under the Secret name its Session already references, before the message is sent.
func TestATaskSessionGetsItsCredentialBackBeforeItsNextMessage(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.withCredentials()
	e.run()
	sid := e.taskSession()
	first := e.grantNative(a, sid)
	ctx := context.Background()
	if _, err := a.Creds.Revoke(ctx, sid); err != nil {
		t.Fatal(err)
	}
	// The cleanup loop deletes the revoked Secret, which frees its name.
	if err := a.Cluster.CoreV1().Secrets(authtest.Namespace).Delete(ctx, first.SecretName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE credentials SET state='deleted' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}

	e.waitState(e.sendTo(sid, "one more thing"), dbstate.DeliveryCompleted)
	var live int
	for _, c := range e.credentialRows(sid) {
		if c.State == dbstate.CredentialPublished {
			live++
			if c.SecretName != first.SecretName {
				t.Errorf("Secret %q, want the same name %q", c.SecretName, first.SecretName)
			}
		}
	}
	if live != 1 {
		t.Fatalf("published credentials = %d, want 1: %+v", live, e.credentialRows(sid))
	}
	if a.Cluster.UID(authtest.Namespace, first.SecretName) == "" {
		t.Error("the Secret is not in the cluster")
	}
}
