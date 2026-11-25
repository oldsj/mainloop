package privileged_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/approval"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/devices"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

const testOrigin = "https://mainloop.test"

// phone is a registered device with a request key and an approval key (software keys standing in
// for the Secure Enclave's).
type phone struct {
	id       string
	approval *ecdsa.PrivateKey
}

func coords(k *ecdsa.PublicKey) (string, string) {
	b, err := k.Bytes() // 0x04 || X || Y
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[1:33]), base64.RawURLEncoding.EncodeToString(b[33:])
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *env) registerPhone(t *testing.T, n int) phone {
	t.Helper()
	req, appr := newKey(t), newKey(t)
	rx, ry := coords(&req.PublicKey)
	ax, ay := coords(&appr.PublicKey)
	at := devices.Thumbprint(ax, ay)
	id := fmt.Sprintf("dev_%026d", n)
	if _, err := dbq.New(e.db.Pool).InsertOwnerDevice(context.Background(), dbq.InsertOwnerDeviceParams{
		ID: id, OwnerID: owner, Name: "Fictional iPhone", Platform: "ios",
		RequestKeyX: rx, RequestKeyY: ry, KeyThumbprint: devices.Thumbprint(rx, ry), RegisteredBy: "owner-subject",
		RegistrationAuthTime: time.Now(), ApprovalKeyX: &ax, ApprovalKeyY: &ay, ApprovalKeyThumbprint: &at,
	}); err != nil {
		t.Fatal(err)
	}
	return phone{id: id, approval: appr}
}

func (p phone) pinned() approval.Pinned {
	return approval.Pinned{p.id: &p.approval.PublicKey}
}

func (p phone) ctx() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindDevice, Name: p.id, Scope: auth.ScopeOwner, Via: "device"})
}

type signOpts struct {
	actionID, digest, jti string
	iat                   time.Time
	key                   *ecdsa.PrivateKey
}

// sign makes the token the app makes (ios/Packages/MainloopKit, ApprovalToken).
func (p phone) sign(t *testing.T, o signOpts) string {
	t.Helper()
	if o.iat.IsZero() {
		o.iat = time.Now()
	}
	if o.jti == "" {
		o.jti = "jti-" + fmt.Sprint(o.iat.UnixNano())
	}
	if o.key == nil {
		o.key = p.approval
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": auth.DeviceIssuer, "sub": p.id, "aud": testOrigin, "iat": o.iat.Unix(), "exp": o.iat.Add(2 * time.Minute).Unix(),
		"jti": o.jti, "act": o.actionID, "dig": o.digest,
	})
	tok.Header["typ"] = approval.TokenType
	tok.Header["kid"] = p.id
	raw, err := tok.SignedString(o.key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// approveOnPhone answers the card from the phone with Approve, the digest and a token.
func (e *env) approveOnPhone(p phone, a privileged.Action, token string) error {
	meta := map[string]any{"reviewed_context": a.Digest, approval.MetadataKey: token}
	return e.inbox.Respond(p.ctx(), a.QueueItemID, api.QueueItemResponse{Response: privileged.AnswerApprove, Metadata: &meta})
}

// checker is serve-api's approval check, with pinned keys as the executor has them.
func checker(pinned approval.Pinned) *approval.Checker {
	return &approval.Checker{
		OwnerID: owner, Audience: testOrigin, Pinned: pinned,
		ExecutorTypes: map[string]bool{privileged.TypeItemToSecret: true},
	}
}

// pendingCopy requests an executor type and brings it to its open card.
func (e *execEnv) pendingCopy(t *testing.T, requestID, secret string) privileged.Action {
	t.Helper()
	a := e.requestCopy(t, requestID, copyArgs(appNS, secret, "password"))
	e.runExecutor(t)
	e.run(t)
	return e.get(t, a.ID)
}

func forbiddenErr(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, attention.ErrForbidden) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want forbidden %q", err, want)
	}
}

// Given a phone whose approval key the executor pins, when the owner approves an executor type on
// the phone with Touch ID, then serve-api records the signed approval, the executor checks it
// against its pinned key and runs the action, and the audit shows which phone signed.
func TestAPinnedPhoneApprovesAndTheExecutorRuns(t *testing.T) {
	e := newExecEnv(t)
	p := e.registerPhone(t, 1)
	e.inbox.Approvals = checker(p.pinned())
	e.ex.ApprovalKeys = p.pinned()

	a := e.pendingCopy(t, "copy-1", "widgets-db")
	if err := e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest})); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if got := e.get(t, a.ID); got.State != privileged.StateApproved {
		t.Fatalf("after the phone's approval: %+v", got)
	}
	e.runExecutor(t)
	if got := e.get(t, a.ID); got.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", got, deref(got.LastError))
	}
	d, err := (&privileged.PrivilegedAPI{Pool: e.db.Pool, OwnerID: owner}).GetPrivilegedAction(context.Background(), api.GetPrivilegedActionRequestObject{ActionId: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.(api.GetPrivilegedAction200JSONResponse).Approval; got == nil || got.DeviceId != p.id {
		t.Fatalf("audit approval = %+v", got)
	}
}

// Given the executor pins approval keys, when serve-api records an approval no pinned key signed
// (a compromised or misconfigured serve-api), then the executor runs nothing.
func TestTheExecutorRunsOnlyWhatAPinnedKeySigned(t *testing.T) {
	cases := []struct {
		name, want string
		approve    func(t *testing.T, e *execEnv, p, other phone, a privileged.Action)
	}{
		{"an unsigned web approval", "not signed with a Touch ID approval key", func(t *testing.T, e *execEnv, _, _ phone, a privileged.Action) {
			e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
		}},
		{"a phone whose key is not pinned", "did not verify against a pinned key", func(t *testing.T, e *execEnv, _, other phone, a privileged.Action) {
			if err := e.approveOnPhone(other, a, other.sign(t, signOpts{actionID: a.ID, digest: a.Digest})); err != nil {
				t.Fatal(err)
			}
		}},
		{"a token forged with another key under the pinned phone's id", "did not verify against a pinned key", func(t *testing.T, e *execEnv, p, _ phone, a privileged.Action) {
			forged := p.sign(t, signOpts{actionID: a.ID, digest: a.Digest, key: newKey(t)})
			e.recordApproval(t, a.ID, p.id, "forged-jti-0000001", forged)
			e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newExecEnv(t)
			p, other := e.registerPhone(t, 1), e.registerPhone(t, 2)
			e.inbox.Approvals = checker(nil) // serve-api enforces no pins
			e.ex.ApprovalKeys = p.pinned()
			a := e.pendingCopy(t, "copy-1", "widgets-db")
			c.approve(t, e, p, other, a)
			e.run(t)
			if got := e.get(t, a.ID); got.State != privileged.StateApproved {
				t.Fatalf("serve-api did not approve: %+v", got)
			}
			e.runExecutor(t)
			got := e.get(t, a.ID)
			if got.State != privileged.StateFailed || !strings.Contains(deref(got.LastError), c.want) || e.op.Reads != 0 {
				t.Fatalf("state %s, error %q, reads %d; want failed with %q", got.State, deref(got.LastError), e.op.Reads, c.want)
			}
		})
	}
}

func (e *env) recordApproval(t *testing.T, actionID, deviceID, jti, token string) {
	t.Helper()
	if _, err := dbq.New(e.db.Pool).RecordPrivilegedApproval(context.Background(), dbq.RecordPrivilegedApprovalParams{
		ActionID: actionID, DeviceID: deviceID, Jti: jti, Token: token, SignedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// Given a signed approval of one action, when the stored row is changed under the same id, its
// digest recomputed to match (what a compromised serve-api could write), then the signature's
// digest no longer matches and nothing runs.
func TestASwappedRowCannotReuseTheSignature(t *testing.T) {
	e := newExecEnv(t)
	p := e.registerPhone(t, 1)
	e.inbox.Approvals = checker(p.pinned())
	e.ex.ApprovalKeys = p.pinned()
	a := e.pendingCopy(t, "copy-1", "widgets-db")
	if err := e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest})); err != nil {
		t.Fatal(err)
	}
	e.run(t)

	// The swap: other arguments, and a digest made over them, approved as such. Triggers guard
	// those columns, but serve-api's role owns the table and could turn them off.
	e.exec(t, `ALTER TABLE privileged_actions DISABLE TRIGGER USER`)
	t.Cleanup(func() { e.exec(t, `ALTER TABLE privileged_actions ENABLE TRIGGER USER`) })
	e.exec(t, `UPDATE privileged_actions SET args = $2 WHERE id = $1`, a.ID, copyArgs(appNS, "other-secret", "password"))
	row, err := dbq.New(e.db.Pool).GetPrivilegedAction(context.Background(), dbq.GetPrivilegedActionParams{OwnerID: owner, ID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	swapped, err := privileged.Digest(row, row.FrozenFacts)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE privileged_actions SET digest = $2, approved_digest = $2 WHERE id = $1`, a.ID, swapped)

	e.runExecutor(t)
	got := e.get(t, a.ID)
	if got.State != privileged.StateFailed || !strings.Contains(deref(got.LastError), "names another action or digest") || e.op.Reads != 0 {
		t.Fatalf("state %s, error %q, reads %d", got.State, deref(got.LastError), e.op.Reads)
	}
}

// Given the executor's record of the tokens it acted on, a token used for another action is
// refused, one first acted on more than 15 minutes after it was signed is refused, and a retry of
// the same action, which finds its own record, is not bound by that age.
func TestTheExecutorUsesATokenOnce(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, e *execEnv, a privileged.Action, jti string)
		iat   time.Time
		want  string // "" runs
	}{
		{"used for another action", func(t *testing.T, e *execEnv, _ privileged.Action, jti string) {
			e.exec(t, `INSERT INTO executor_approval_uses (jti, action_id) VALUES ($1, 'pa_other')`, jti)
		}, time.Now(), "already used for action pa_other"},
		{"signed too long before the first attempt", nil, time.Now().Add(-20 * time.Minute), "older than 15 minutes"},
		{"a retry of its own action", func(t *testing.T, e *execEnv, a privileged.Action, jti string) {
			e.exec(t, `INSERT INTO executor_approval_uses (jti, action_id) VALUES ($1, $2)`, jti, a.ID)
		}, time.Now().Add(-20 * time.Minute), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newExecEnv(t)
			p := e.registerPhone(t, 1)
			e.inbox.Approvals = checker(nil)
			e.ex.ApprovalKeys = p.pinned()
			a := e.pendingCopy(t, "copy-1", "widgets-db")
			jti := "executor-jti-000001"
			// Recorded directly: serve-api would refuse a token this old when it arrived.
			e.recordApproval(t, a.ID, p.id, jti, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest, jti: jti, iat: c.iat}))
			e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
			e.run(t)
			if c.setup != nil {
				c.setup(t, e, a, jti)
			}
			e.runExecutor(t)
			got := e.get(t, a.ID)
			if c.want == "" {
				if got.State != privileged.StateSucceeded {
					t.Fatalf("state %s, error %q", got.State, deref(got.LastError))
				}
				return
			}
			if got.State != privileged.StateFailed || !strings.Contains(deref(got.LastError), c.want) {
				t.Fatalf("state %s, error %q; want failed with %q", got.State, deref(got.LastError), c.want)
			}
		})
	}
}

// Given serve-api's check of a phone's Approve on a privileged card, it is refused unless a token
// the phone's approval key signed for this card's action and digest comes with it, fresh and
// unused; and with keys pinned, an executor type is approved only on a pinned phone.
func TestServeAPIChecksThePhonesSignature(t *testing.T) {
	e := newExecEnv(t)
	p, other := e.registerPhone(t, 1), e.registerPhone(t, 2)
	e.inbox.Approvals = checker(p.pinned())
	a := e.pendingCopy(t, "copy-1", "widgets-db")
	b := e.pendingCopy(t, "copy-2", "widgets-cache")

	refused := []struct {
		name, want string
		send       func() error
	}{
		{"no token", "needs its Touch ID signature", func() error {
			meta := map[string]any{"reviewed_context": a.Digest}
			return e.inbox.Respond(p.ctx(), a.QueueItemID, api.QueueItemResponse{Response: "approve", Metadata: &meta})
		}},
		{"signed for another action", "another action or digest", func() error {
			return e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: b.ID, digest: a.Digest}))
		}},
		{"signed for another digest", "another action or digest", func() error {
			return e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: b.Digest}))
		}},
		{"expired", "expired", func() error {
			return e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest, iat: time.Now().Add(-10 * time.Minute)}))
		}},
		{"signed by another phone's key", "verification error", func() error {
			return e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest, key: other.approval}))
		}},
		{"a phone whose key is not pinned", "not pinned", func() error {
			return e.approveOnPhone(other, a, other.sign(t, signOpts{actionID: a.ID, digest: a.Digest}))
		}},
		{"the web app, for an executor type while keys are pinned", "approve this on your iPhone", func() error {
			meta := map[string]any{"reviewed_context": a.Digest}
			return e.inbox.Respond(context.Background(), a.QueueItemID, api.QueueItemResponse{Response: privileged.AnswerApprove, Metadata: &meta})
		}},
	}
	for _, c := range refused {
		forbiddenErr(t, c.send(), c.want)
	}

	if err := e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest, jti: "once-only-jti-0001"})); err != nil {
		t.Fatal(err)
	}
	// The same token again, now for the other card, is refused whatever it names.
	err := e.approveOnPhone(p, b, p.sign(t, signOpts{actionID: b.ID, digest: b.Digest, jti: "once-only-jti-0001"}))
	forbiddenErr(t, err, "already used")
	// Deny from the phone needs no signature.
	if err := e.inbox.Respond(p.ctx(), b.QueueItemID, api.QueueItemResponse{Response: privileged.AnswerDeny}); err != nil {
		t.Fatal(err)
	}
}

// Given no keys pinned, a phone's signed Approve approves a type serve-api runs itself, and a
// phone without an approval key is told to sign in again.
func TestAPhoneApprovesAMergeWithItsSignature(t *testing.T) {
	e := newEnv(t)
	e.inbox.Approvals = checker(nil)
	p := e.registerPhone(t, 1)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "merge-1")
	e.run(t)
	a = e.get(t, a.ID)

	old := fmt.Sprintf("dev_%026d", 9)
	k := newKey(t)
	x, y := coords(&k.PublicKey)
	if _, err := dbq.New(e.db.Pool).InsertOwnerDevice(context.Background(), dbq.InsertOwnerDeviceParams{
		ID: old, OwnerID: owner, Name: "Older iPhone", Platform: "ios", RequestKeyX: x, RequestKeyY: y,
		KeyThumbprint: devices.Thumbprint(x, y), RegisteredBy: "owner-subject", RegistrationAuthTime: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	keyless := phone{id: old, approval: k}
	forbiddenErr(t, e.approveOnPhone(keyless, a, keyless.sign(t, signOpts{actionID: a.ID, digest: a.Digest})), "sign out and in again")

	if err := e.approveOnPhone(p, a, p.sign(t, signOpts{actionID: a.ID, digest: a.Digest})); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	e.run(t)
	if got := e.get(t, a.ID); got.State != privileged.StateSucceeded {
		t.Fatalf("after the phone's approval: %+v (%v)", got, deref(got.LastError))
	}
}
