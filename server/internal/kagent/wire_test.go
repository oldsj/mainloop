package kagent_test

import (
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/kagent"
	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// rawSessionServer answers every call with the given Session bytes inside a SessionResponse,
// framed as gRPC-web, so a test controls the wire a decoded message would hide.
func rawSessionServer(t *testing.T, session []byte) *kagent.Client {
	t.Helper()
	body := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), session)
	frame := append([]byte{0, 0, 0, 0, 0}, body...)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
	trailer := []byte("grpc-status: 0\r\n")
	tf := append([]byte{0x80, 0, 0, 0, 0}, trailer...)
	binary.BigEndian.PutUint32(tf[1:5], uint32(len(trailer)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		_, _ = w.Write(append(frame, tf...))
	}))
	t.Cleanup(srv.Close)
	c, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func field(num protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), b)
}

func varint(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}

// Python's test_malformed_preparation_wire_still_uses_strict_client_validation: a reply that
// repeats the workspace preparation, or the runtime association, is ambiguous and is refused before
// protobuf merges the copies into one message nobody sent.
func TestASessionReplyThatRepeatsAPreparationFieldIsRefusedOnTheWire(t *testing.T) {
	base := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1"})
	receipt := marshal(t, &api.WorkspacePreparationReceipt{
		Original:       &api.PrepareSessionWorkspaceRequest{SessionId: "s-1", ActionId: "first"},
		ContextId:      "s-1",
		Classification: "pending",
	})
	second := marshal(t, &api.WorkspacePreparationReceipt{
		Original:       &api.PrepareSessionWorkspaceRequest{SessionId: "s-1"},
		ContextId:      "s-1",
		Classification: "confirmed",
	})
	assoc := marshal(t, &api.RuntimeAssociation{GenerationId: "g", ActorUid: "u", Phase: "active"})
	cat := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	refused := map[string][]byte{
		"duplicate receipt":             cat(base, field(21, receipt), field(21, second)),
		"duplicate empty receipt":       cat(base, field(21, nil), field(21, nil)),
		"duplicate association":         cat(base, field(20, assoc), field(20, assoc)),
		"duplicate receipt scalar":      cat(base, field(21, cat(receipt, field(7, []byte("confirmed"))))),
		"duplicate original field":      cat(base, field(21, cat(field(1, cat(marshal(t, &api.PrepareSessionWorkspaceRequest{SessionId: "s-1"}), field(1, []byte("again")))), field(4, []byte("s-1"))))),
		"duplicate original":            cat(base, field(21, cat(receipt, field(1, nil)))),
		"duplicate observed timestamp":  cat(base, field(21, cat(receipt, field(17, marshal(t, &timestamppb.Timestamp{Seconds: 1})), field(17, marshal(t, &timestamppb.Timestamp{Seconds: 2}))))),
		"duplicate sequence (varint)":   cat(base, field(21, cat(receipt, varint(9, 1), varint(9, 2)))),
		"duplicate nested timestamp":    cat(base, field(21, cat(receipt, field(16, cat(varint(1, 1), varint(1, 2)))))),
		"duplicate within the receipts": cat(base, field(21, cat(receipt, receipt))),
	}
	for name, session := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := rawSessionServer(t, session).GetSession(ctx, "s-1")
			var unknown *kagent.OutcomeUnknown
			if !errors.As(err, &unknown) {
				t.Fatalf("err = %v, want *OutcomeUnknown", err)
			}
		})
	}
	accepted := map[string][]byte{
		"no preparation":     base,
		"one receipt":        cat(base, field(21, receipt)),
		"one empty receipt":  cat(base, field(21, nil)),
		"one association":    cat(base, field(20, assoc)),
		"receipt and assoc.": cat(base, field(21, receipt), field(20, assoc)),
	}
	for name, session := range accepted {
		t.Run(name, func(t *testing.T) {
			s, err := rawSessionServer(t, session).GetSession(ctx, "s-1")
			if err != nil || s.GetId() != "s-1" {
				t.Fatalf("session = %v, err = %v", s, err)
			}
		})
	}
}

// ObserveSession shows a Session whose context is unstated; GetSession, which acts are built on,
// still refuses it, and neither accepts a context naming something else.
func TestObserveSessionAcceptsAnUnstatedContextAndGetSessionDoesNot(t *testing.T) {
	unstated := marshal(t, &api.Session{Id: "s-1"})
	foreign := marshal(t, &api.Session{Id: "s-1", ContextId: "other"})
	wrongID := marshal(t, &api.Session{Id: "other", ContextId: "other"})

	s, err := rawSessionServer(t, unstated).ObserveSession(ctx, "s-1")
	if err != nil || s.GetId() != "s-1" || s.GetContextId() != "" {
		t.Fatalf("observe unstated = %v, %v", s, err)
	}
	var unknown *kagent.OutcomeUnknown
	if _, err := rawSessionServer(t, unstated).GetSession(ctx, "s-1"); !errors.As(err, &unknown) {
		t.Errorf("GetSession accepted an unstated context: %v", err)
	}
	for name, raw := range map[string][]byte{"foreign context": foreign, "other session": wrongID} {
		if _, err := rawSessionServer(t, raw).ObserveSession(ctx, "s-1"); !errors.As(err, &unknown) {
			t.Errorf("ObserveSession accepted %s: %v", name, err)
		}
	}
}
