package sessions_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// grpcWebReply frames a response body as a successful gRPC-web unary reply.
func grpcWebReply(body []byte) []byte {
	frame := append([]byte{0, 0, 0, 0, 0}, body...)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
	trailer := []byte("grpc-status: 0\r\n")
	tf := append([]byte{0x80, 0, 0, 0, 0}, trailer...)
	binary.BigEndian.PutUint32(tf[1:5], uint32(len(trailer)))
	return append(frame, tf...)
}

// answerGetSessionRaw puts a proxy between the service and the fake kagent that answers GetSession
// with the given response bytes and passes every other call through. It returns the number of
// GetSession calls the proxy has answered.
func (p *prep) answerGetSessionRaw(t *testing.T, body []byte) *atomic.Int32 {
	t.Helper()
	upstream, err := url.Parse(p.e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	pass := httputil.NewSingleHostReverseProxy(upstream)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != kagent.ProcGetSession {
			pass.ServeHTTP(w, r)
			return
		}
		gets.Add(1)
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		_, _ = w.Write(grpcWebReply(body))
	}))
	t.Cleanup(proxy.Close)
	c, err := kagent.New(kagent.Config{GatewayURL: proxy.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	p.e.svc.Kagent = c
	return &gets
}

func (p *prep) readRaw(t *testing.T, body []byte) *api.NativeSessionInfo {
	t.Helper()
	gets := p.answerGetSessionRaw(t, body)
	resp, err := p.e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: p.sid})
	if err != nil {
		t.Fatal(err)
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("the read made %d GetSession calls, want exactly one", n)
	}
	info := api.NativeSessionInfo(resp.(api.GetSessionNative200JSONResponse))
	return &info
}

// Review probe for G6g re-review 2 P2-1: a reply that splits the singular GetSessionResponse.session
// across two field-1 copies must not merge their receipts into one nobody sent. The first copy
// holds a complete pending receipt; the second holds a receipt that says only "confirmed". Merged,
// they would read as a confirmed receipt with the first one's revision, challenge and timestamps.
func TestASplitSessionEnvelopeDoesNotInventAReceipt(t *testing.T) {
	p := newPrep(t)
	ksid := p.e.bindingKsid(p.sid)
	first := &kagentapi.Session{
		Id: ksid, ContextId: ksid, PreparedRevision: "fixture-revision",
		State:     kagentapi.RuntimeState_RUNTIME_STATE_READY,
		Operation: kagentapi.RuntimeOperation_RUNTIME_OPERATION_NONE,
	}
	withAssociation(first)
	first.WorkspacePreparation = &kagentapi.WorkspacePreparationReceipt{
		Original: &kagentapi.PrepareSessionWorkspaceRequest{
			SessionId: ksid, ActionId: "action-1", PreparedRevision: "revision-from-first-receipt", SetupProfile: "agent",
		},
		ContextId: ksid, Classification: "pending", ChallengeId: "challenge-fixture",
		ObservedAt: &timestamppb.Timestamp{Seconds: 1_700_000_001},
	}
	second := &kagentapi.Session{Id: ksid, ContextId: ksid}
	second.WorkspacePreparation = &kagentapi.WorkspacePreparationReceipt{
		Original:  &kagentapi.PrepareSessionWorkspaceRequest{SessionId: ksid},
		ContextId: ksid, Classification: "confirmed",
	}
	envelope := func(sessions ...*kagentapi.Session) []byte {
		var b []byte
		for _, s := range sessions {
			b = protowire.AppendBytes(protowire.AppendTag(b, 1, protowire.BytesType), mustMarshal(t, s))
		}
		return b
	}

	// A single Session in the envelope is shown, so a refusal below is the split, not the harness.
	one := p.readRaw(t, envelope(first))
	if o := one.PreparationObservation; o == nil || o.ReceiptStatus != "observed" || o.Receipt == nil || string(*o.Receipt.Classification) != "pending" {
		t.Fatalf("one copy: %+v", o)
	}

	split := p.readRaw(t, envelope(first, second))
	if split.PreparationObservation != nil || split.SessionState != nil || split.Note == nil {
		t.Fatalf("a split envelope was shown: %+v note=%v", split.PreparationObservation, split.Note)
	}
	if strings.Contains(string(mustJSON(t, split)), "revision-from-first-receipt") {
		t.Fatal("the first receipt's revision reached the response")
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
