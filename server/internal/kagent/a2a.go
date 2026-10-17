package kagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// The A2A endpoint is JSON-RPC over HTTP with SSE for streams. It uses a2a-go's types but not
// its client: that client drops the HTTP status, the ErrorInfo domain and reason, and whether
// the request left, and every retry decision here depends on those.

const (
	methodSend      = "SendStreamingMessage"
	methodSubscribe = "SubscribeToTask"
	methodGetTask   = "GetTask"
	methodCancel    = "CancelTask"
	methodListTasks = "ListTasks"

	maxReplyBytes = 32 << 20
	sendBackoff   = 100 * time.Millisecond
)

// SendParams is one user message. MessageID is the Mainloop message id and is the only handle
// for finding the message again, so it must be stable across a retry.
type SendParams struct {
	Text      string
	MessageID string
	ContextID string
	TaskID    string // empty starts a new task

	// Metadata and Extensions go on the a2a.Message as given; the HITL response is the one user of
	// them. Extensions name the A2A extensions the message uses.
	Metadata   map[string]any
	Extensions []string
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func (c *Client) newRPCRequest(
	ctx context.Context, agent AgentRef, method string, params any, stream bool,
) (*http.Request, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: uuid.NewString(), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+agent.path(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream, application/json")
	}
	return req, nil
}

// parseEnvelope turns a JSON-RPC reply into its result or a typed error. status is the HTTP
// status, used only when the body is not an envelope.
func parseEnvelope(method string, status int, body []byte) (json.RawMessage, error) {
	var env rpcEnvelope
	if json.Unmarshal(body, &env) == nil {
		if env.Error != nil {
			return nil, classifyRPCError(env.Error.Code, env.Error.Message, env.Error.Data)
		}
		if len(env.Result) > 0 && !bytes.Equal(env.Result, []byte("null")) {
			return env.Result, nil
		}
		if status == http.StatusOK {
			return nil, &OutcomeUnknown{Op: "A2A " + method, Detail: "reply had no result"}
		}
	}
	if status >= http.StatusInternalServerError {
		return nil, &OutcomeUnknown{Op: "A2A " + method, Detail: "gateway error HTTP " + strconv.Itoa(status)}
	}
	return nil, &ProtocolError{Op: "A2A " + method, Status: status}
}

func authStatus(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authRefused("HTTP "+strconv.Itoa(resp.StatusCode), resp.StatusCode == http.StatusUnauthorized)
	}
	return nil
}

// call runs one non-streaming JSON-RPC method and returns its result.
func (c *Client) call(ctx context.Context, agent AgentRef, method string, params any) (json.RawMessage, error) {
	op := "A2A " + method
	ctx, cancel := context.WithTimeout(ctx, c.reqTimeout)
	defer cancel()
	req, err := c.newRPCRequest(ctx, agent, method, params, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransport(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := authStatus(resp); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
	if err != nil || len(body) > maxReplyBytes {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply unreadable", Err: err}
	}
	return parseEnvelope(method, resp.StatusCode, body)
}

// GetTask reads one task, with its artifacts and the whole history.
func (c *Client) GetTask(ctx context.Context, agent AgentRef, taskID string) (*a2a.Task, error) {
	raw, err := c.call(ctx, agent, methodGetTask, a2a.GetTaskRequest{ID: a2a.TaskID(taskID)})
	if err != nil {
		return nil, err
	}
	var task a2a.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, &OutcomeUnknown{Op: "A2A " + methodGetTask, Detail: "bad task", Err: err}
	}
	return &task, nil
}

// CancelTask asks kagent to stop a task and returns the task as kagent reports it afterwards. A
// task that already ended is returned unchanged, so the caller reads the state, not the absence
// of an error, to learn whether the cancel took. Cancelling is idempotent, so a lost reply is
// settled by calling again or by GetTask.
func (c *Client) CancelTask(ctx context.Context, agent AgentRef, taskID string) (*a2a.Task, error) {
	raw, err := c.call(ctx, agent, methodCancel, a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)})
	if err != nil {
		return nil, err
	}
	var task a2a.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, &OutcomeUnknown{Op: "A2A " + methodCancel, Detail: "bad task", Err: err}
	}
	return &task, nil
}

// ListTasksPage reads one page of a conversation's tasks. A listed task carries no artifacts.
func (c *Client) ListTasksPage(
	ctx context.Context, agent AgentRef, contextID, token string, limit int,
) (tasks []*a2a.Task, next string, err error) {
	raw, err := c.call(ctx, agent, methodListTasks, a2a.ListTasksRequest{
		ContextID: contextID,
		PageSize:  min(max(limit, 1), maxPageSize),
		PageToken: token,
	})
	if err != nil {
		return nil, "", err
	}
	var resp a2a.ListTasksResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", &OutcomeUnknown{Op: "A2A " + methodListTasks, Detail: "bad task list", Err: err}
	}
	return resp.Tasks, resp.NextPageToken, nil
}

// ListWaitingTasksPage reads one page of the tasks of a conversation that wait for input, each with
// at most its latest history message. A waiting task's ask is in its status message, so this is
// all a watcher needs and it stays small however long the conversation is; read a task with
// GetTask for its whole history.
func (c *Client) ListWaitingTasksPage(
	ctx context.Context, agent AgentRef, contextID, token string, limit int,
) (tasks []*a2a.Task, next string, err error) {
	history := 1
	raw, err := c.call(ctx, agent, methodListTasks, a2a.ListTasksRequest{
		ContextID:     contextID,
		Status:        a2a.TaskStateInputRequired,
		HistoryLength: &history,
		PageSize:      min(max(limit, 1), maxPageSize),
		PageToken:     token,
	})
	if err != nil {
		return nil, "", err
	}
	var resp a2a.ListTasksResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", &OutcomeUnknown{Op: "A2A " + methodListTasks, Detail: "bad task list", Err: err}
	}
	return resp.Tasks, resp.NextPageToken, nil
}

// ListTasks reads every task in a conversation.
func (c *Client) ListTasks(ctx context.Context, agent AgentRef, contextID string) ([]*a2a.Task, error) {
	return paginate("A2A "+methodListTasks, func(token string) ([]*a2a.Task, string, error) {
		return c.ListTasksPage(ctx, agent, contextID, token, maxPageSize)
	})
}

// ListTasksLatestPage reads one page of a conversation's tasks, each with at most its newest history
// message and without its artifacts. The status and that message say what a task is and whether it
// has finished, and they stay small however long the conversation is: the artifacts of a long
// conversation (every streamed text delta, every tool result in full) can run to megabytes, so they
// are read only for the tasks that can still change, with [Client.ListTasksInStateLatest].
func (c *Client) ListTasksLatestPage(
	ctx context.Context, agent AgentRef, contextID, token string, limit int,
) (tasks []*a2a.Task, next string, err error) {
	return c.listTasksLatest(ctx, agent, a2a.ListTasksRequest{ContextID: contextID, PageToken: token}, limit)
}

// ListTasksLatest reads every task in a conversation with at most its newest history message and
// without its artifacts.
func (c *Client) ListTasksLatest(ctx context.Context, agent AgentRef, contextID string) ([]*a2a.Task, error) {
	return paginate("A2A "+methodListTasks, func(token string) ([]*a2a.Task, string, error) {
		return c.ListTasksLatestPage(ctx, agent, contextID, token, maxPageSize)
	})
}

// ListTasksInStateLatest reads every task of a conversation that is in state, each with at most its
// newest history message and with its artifacts. A harness turn records its text and tool calls as
// artifact events, which add no history and do not move the status time, so the artifacts are the
// only record of a turn that is busy; a liveness sweep reads them for the working and submitted
// tasks only.
func (c *Client) ListTasksInStateLatest(
	ctx context.Context, agent AgentRef, contextID string, state a2a.TaskState,
) ([]*a2a.Task, error) {
	return paginate("A2A "+methodListTasks, func(token string) ([]*a2a.Task, string, error) {
		return c.listTasksLatest(ctx, agent, a2a.ListTasksRequest{
			ContextID: contextID, Status: state, IncludeArtifacts: true, PageToken: token,
		}, maxPageSize)
	})
}

func (c *Client) listTasksLatest(
	ctx context.Context, agent AgentRef, req a2a.ListTasksRequest, limit int,
) (tasks []*a2a.Task, next string, err error) {
	history := 1
	req.HistoryLength = &history
	req.PageSize = min(max(limit, 1), maxPageSize)
	raw, err := c.call(ctx, agent, methodListTasks, req)
	if err != nil {
		return nil, "", err
	}
	var resp a2a.ListTasksResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", &OutcomeUnknown{Op: "A2A " + methodListTasks, Detail: "bad task list", Err: err}
	}
	return resp.Tasks, resp.NextPageToken, nil
}

// FindTaskForMessage resolves an uncertain send: it returns the task whose history holds the
// message id, read again with GetTask so the artifacts are included, or found=false when no
// task has it. If the message is found but the re-read fails, it returns found=true with the
// error and a nil task. It never sends anything.
func (c *Client) FindTaskForMessage(
	ctx context.Context, agent AgentRef, contextID, messageID string,
) (task *a2a.Task, found bool, err error) {
	tasks, err := c.ListTasks(ctx, agent, contextID)
	if err != nil {
		return nil, false, err
	}
	for _, t := range tasks {
		for _, m := range t.History {
			if m != nil && m.ID == messageID {
				// found stays true when the re-read fails: the message exists, so the caller must
				// not conclude it is missing and send it again.
				full, err := c.GetTask(ctx, agent, string(t.ID))
				return full, true, err
			}
		}
	}
	return nil, false, nil
}

// SubscribeToTask reconnects to a task's events. The first event is the current task. The
// sequence is single-use: ranging over it a second time yields an error and sends nothing.
func (c *Client) SubscribeToTask(ctx context.Context, agent AgentRef, taskID string) iter.Seq2[a2a.Event, error] {
	return singleUse(func(yield func(a2a.Event, error) bool) {
		_, _, err := c.streamOnce(ctx, agent, methodSubscribe, a2a.SubscribeToTaskRequest{ID: a2a.TaskID(taskID)}, yield)
		if err != nil {
			yield(nil, err)
		}
	})
}

// SendMessage sends one user message and streams the task's events.
//
// If kagent refuses the message before any event (SendNotAccepted), the identical message is
// sent again until the retry budget runs out. Nothing else is ever sent twice: a failure after
// the request left is yielded as an error, and the caller observes with FindTaskForMessage.
//
// The sequence is single-use: kagent does not dedupe a message id once its task has completed,
// so ranging over it a second time yields an error and sends nothing. To send again, call
// SendMessage again, and only when NotSent says the first send did not happen.
func (c *Client) SendMessage(ctx context.Context, agent AgentRef, p SendParams) iter.Seq2[a2a.Event, error] {
	return singleUse(func(yield func(a2a.Event, error) bool) {
		if p.Text == "" || p.MessageID == "" || p.ContextID == "" {
			yield(nil, errors.New("kagent: a message needs text, a message id and a context id"))
			return
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(p.Text))
		msg.ID, msg.ContextID, msg.TaskID = p.MessageID, p.ContextID, a2a.TaskID(p.TaskID)
		msg.Metadata, msg.Extensions = p.Metadata, p.Extensions
		params := a2a.SendMessageRequest{Message: msg}

		deadline := c.now().Add(c.budget)
		for attempt := 1; ; attempt++ {
			started, stopped, err := c.streamOnce(ctx, agent, methodSend, params, yield)
			if stopped || err == nil {
				return
			}
			var refused *SendNotAccepted
			if started || !errors.As(err, &refused) {
				yield(nil, err)
				return
			}
			// kagent's hint, backed off so fast refusals cannot spin through the budget.
			delay := max(refused.RetryAfter(), min(sendBackoff<<min(attempt-1, 10), sendBackoffCap))
			if !c.now().Add(delay).Before(deadline) {
				yield(nil, err)
				return
			}
			if serr := c.sleep(ctx, delay); serr != nil {
				// The wait was cut short: the last refusal says why the send never started, which the
				// context error does not.
				yield(nil, err)
				return
			}
		}
	})
}

var errSequenceUsed = errors.New("kagent: this event sequence was already consumed; it sends nothing a second time")

// singleUse lets a sequence run once. A second range yields errSequenceUsed and does no work.
func singleUse(seq iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	var used atomic.Bool
	return func(yield func(a2a.Event, error) bool) {
		if !used.CompareAndSwap(false, true) {
			yield(nil, errSequenceUsed)
			return
		}
		seq(yield)
	}
}
