package kagent

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// errAmbiguousWire marks a reply whose raw wire repeats a field that must appear once. Protobuf
// decoding would merge the copies into one message nobody sent, so the check runs on the wire
// before decoding. Like any malformed reply it does not establish what the call did.
var errAmbiguousWire = errors.New("ambiguous kagent reply")

// strictProto is the binary protobuf codec for Session replies. It refuses a reply that repeats the
// Session's runtime association or workspace preparation, or any singular field inside a
// preparation receipt, as the Python client does (kagent_client.py, _decode_session_fields and
// _preparation_fields).
type strictProto struct{}

func (strictProto) Name() string { return "proto" }

func (strictProto) Marshal(msg any) ([]byte, error) {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("kagent: %T is not a protobuf message", msg)
	}
	return proto.Marshal(m)
}

func (strictProto) Unmarshal(data []byte, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("kagent: %T is not a protobuf message", msg)
	}
	if err := checkReplyWire(data, m.ProtoReflect().Descriptor()); err != nil {
		return err
	}
	return proto.Unmarshal(data, m)
}

var (
	sessionName = (&api.Session{}).ProtoReflect().Descriptor().FullName()
	receiptName = (&api.WorkspacePreparationReceipt{}).ProtoReflect().Descriptor().FullName()
)

// checkReplyWire looks for the Sessions and receipts a reply carries at its top level. A singular
// Session or receipt field that appears twice is refused, because protobuf would merge the copies
// into a message nobody sent; a repeated field (ListSessions.sessions) may appear any number of
// times. A message it cannot walk is left to the protobuf decoder to reject.
func checkReplyWire(data []byte, md protoreflect.MessageDescriptor) error {
	seen := map[protowire.Number]int{}
	return eachField(data, func(num protowire.Number, payload []byte) error {
		fd := md.Fields().ByNumber(num)
		if fd == nil || fd.Message() == nil {
			return nil
		}
		if name := fd.Message().FullName(); name == sessionName || name == receiptName {
			if seen[num]++; seen[num] > 1 && !fd.IsList() {
				return fmt.Errorf("%w: field %s of %s appears twice", errAmbiguousWire, fd.Name(), md.Name())
			}
		}
		switch fd.Message().FullName() {
		case sessionName:
			return checkSessionWire(payload, fd.Message())
		case receiptName:
			return checkOnce(payload, fd.Message())
		}
		return nil
	})
}

func checkSessionWire(data []byte, md protoreflect.MessageDescriptor) error {
	seen := map[protowire.Number]int{}
	var prep []byte
	err := eachField(data, func(num protowire.Number, payload []byte) error {
		switch fd := md.Fields().ByNumber(num); {
		case fd == nil || fd.Message() == nil:
		case fd.Message().FullName() == receiptName:
			seen[num]++
			prep = payload
		case fd.Name() == "runtime_association":
			seen[num]++
		}
		return nil
	})
	if err != nil {
		return err
	}
	for num, n := range seen {
		if n > 1 {
			return fmt.Errorf("%w: field %d of the Session appears %d times", errAmbiguousWire, num, n)
		}
	}
	if prep != nil {
		return checkOnce(prep, md.Fields().ByName("workspace_preparation").Message())
	}
	return nil
}

// checkOnce rejects a message that repeats a singular field, in it or in the messages it nests.
func checkOnce(data []byte, md protoreflect.MessageDescriptor) error {
	seen := map[protowire.Number]int{}
	err := eachField(data, func(num protowire.Number, payload []byte) error {
		fd := md.Fields().ByNumber(num)
		if fd == nil {
			return nil
		}
		if seen[num]++; seen[num] > 1 && !fd.IsList() {
			return fmt.Errorf("%w: field %s of %s appears twice", errAmbiguousWire, fd.Name(), md.Name())
		}
		if fd.Message() != nil && payload != nil && !fd.IsMap() {
			return checkOnce(payload, fd.Message())
		}
		return nil
	})
	return err
}

// eachField calls fn for every field of a message with its length-delimited payload (nil for other
// wire types). A malformed message ends the walk without error: decoding it fails separately.
func eachField(data []byte, fn func(protowire.Number, []byte) error) error {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil
		}
		data = data[n:]
		m := protowire.ConsumeFieldValue(num, typ, data)
		if m < 0 {
			return nil
		}
		var payload []byte
		if typ == protowire.BytesType {
			payload, _ = protowire.ConsumeBytes(data)
			if payload == nil {
				payload = []byte{}
			}
		}
		if err := fn(num, payload); err != nil {
			return err
		}
		data = data[m:]
	}
	return nil
}

// withStrictSessions makes a session client decode its replies with strictProto.
func withStrictSessions() connect.ClientOption { return connect.WithCodec(strictProto{}) }
