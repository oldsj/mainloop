package kagent

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// streamOnce makes one streaming request and yields its events. started is true once an event
// was yielded, and stopped is true when the consumer stopped early. Anything that goes wrong
// after the request left, with no definite answer from kagent, is an *OutcomeUnknown.
func (c *Client) streamOnce(
	ctx context.Context, agent AgentRef, method string, params any, yield func(a2a.Event, error) bool,
) (started, stopped bool, err error) {
	op := "A2A " + method
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := c.newRPCRequest(ctx, agent, method, params, true)
	if err != nil {
		return false, false, err
	}

	headerWait := time.AfterFunc(c.reqTimeout, cancel)
	resp, err := c.http.Do(req)
	headerWait.Stop()
	if err != nil {
		return false, false, classifyTransport(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := authStatus(resp); err != nil {
		return false, false, err
	}

	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		// A refusal arrives as one JSON-RPC body rather than as an SSE error event.
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
		if rerr != nil || len(body) > maxReplyBytes {
			return false, false, &OutcomeUnknown{Op: op, Detail: "reply unreadable", Err: rerr}
		}
		raw, perr := parseEnvelope(method, resp.StatusCode, body)
		if perr != nil {
			return false, false, perr
		}
		return c.deliver(op, raw, yield)
	}

	idle := time.AfterFunc(c.idle, cancel)
	defer idle.Stop()
	var eventErr error
	readErr := readSSE(resp.Body, func(data []byte) bool {
		idle.Reset(c.idle)
		// A frame that is not a JSON-RPC envelope mid-stream means the outcome is unknown.
		raw, perr := parseEnvelope(method, http.StatusBadGateway, data)
		if perr != nil {
			eventErr = perr
			return false
		}
		var delivered bool
		delivered, stopped, eventErr = c.deliver(op, raw, yield)
		started = started || delivered
		return eventErr == nil && !stopped
	})
	switch {
	case stopped:
		return started, true, nil
	case eventErr != nil:
		return started, false, eventErr
	case readErr != nil:
		return started, false, &OutcomeUnknown{Op: op, Detail: "stream broke: " + errorKind(readErr), Err: readErr}
	}
	return started, false, nil
}

// deliver decodes one result and passes it to the consumer.
func (*Client) deliver(op string, raw json.RawMessage, yield func(a2a.Event, error) bool) (started, stopped bool, err error) {
	ev, derr := decodeEvent(raw)
	if derr != nil {
		return false, false, &OutcomeUnknown{Op: op, Detail: "bad event", Err: derr}
	}
	return true, !yield(ev, nil), nil
}

// decodeEvent reads a stream result, which is a wrapped event or, from GetTask-style replies,
// a bare task.
func decodeEvent(raw json.RawMessage) (a2a.Event, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, err
	}
	_, hasID := keys["id"]
	_, hasStatus := keys["status"]
	_, wrapped := keys["task"]
	if hasID && hasStatus && !wrapped {
		var task a2a.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			return nil, err
		}
		return &task, nil
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, err
	}
	return sr.Event, nil
}
