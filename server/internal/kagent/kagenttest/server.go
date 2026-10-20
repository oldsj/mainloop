// Package kagenttest is an in-process kagent for tests: SessionService and AgentService over
// gRPC-web, and a scripted A2A JSON-RPC and SSE endpoint, with faults that make a call fail
// the ways the real gateway does. It is a contract fake, not a live proof.
package kagenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/kagent"
	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// Request is one request the fake received.
type Request struct {
	// Key is the procedure for gRPC-web calls and the JSON-RPC method for A2A calls.
	Key       string
	AgentPath string // A2A only
	Header    http.Header
	MessageID string // A2A SendStreamingMessage only
	// Metadata and Extensions are those of the sent message (A2A SendStreamingMessage only).
	Metadata   map[string]any
	Extensions []string
}

// Fault makes the next call with a given key fail. Fields are applied in this order: Delay,
// Drop, Status, then the protocol-level error.
type Fault struct {
	Delay  time.Duration
	Drop   bool // close the connection with no response
	Status int  // answer with this bare HTTP status
	// GRPC answers a gRPC-web call with this code in the trailers (HTTP 200).
	GRPC *connect.Code
	// Message is the gRPC status message; empty means "injected <code>".
	Message string
	// RPC answers an A2A call with this JSON-RPC error (HTTP 200).
	RPC *RPCError
	// Truncate runs the call to completion, so its effects commit, then cuts the reply body short: the
	// client sees a reply that fails to decode, not an error kagent sent.
	Truncate bool
}

// RPCError is a JSON-RPC error object with google.rpc.ErrorInfo details.
type RPCError struct {
	Code     int
	Message  string
	Reason   string
	Domain   string
	Metadata map[string]string
}

// NotAccepted is kagent's "input was not accepted, send the same message again" refusal.
func NotAccepted(retryAfterMs int) *RPCError {
	return &RPCError{
		Code: -32004, Message: "input was not accepted", Reason: "UNSUPPORTED_OPERATION",
		Domain:   "a2a-protocol.org",
		Metadata: map[string]string{"reason": "KAGENT_SEND_NOT_ACCEPTED", "retryAfterMs": strconv.Itoa(retryAfterMs)},
	}
}

// NotReady is kagent's refusal of a send to a Session that is suspended or changing: no marker, so
// the client recognises it by code and text.
func NotReady() *RPCError {
	return &RPCError{Code: -32004, Message: "Session cannot accept work during a lifecycle operation"}
}

// Unauthenticated is the a2a-go ErrUnauthenticated wire shape.
func Unauthenticated() *RPCError {
	return &RPCError{Code: -31401, Message: "unauthenticated", Reason: "UNAUTHENTICATED", Domain: "a2a-protocol.org"}
}

// Unauthorized is the a2a-go ErrUnauthorized wire shape.
func Unauthorized() *RPCError {
	return &RPCError{Code: -31403, Message: "permission denied", Reason: "UNAUTHORIZED", Domain: "a2a-protocol.org"}
}

// Step is one part of a scripted SendStreamingMessage reply.
type Step interface{ step() }

type (
	// Emit streams one event.
	Emit struct{ Event a2a.Event }
	// Drop cuts the connection mid-stream.
	Drop struct{}
	// Hang stops sending and holds the stream open until the client goes away.
	Hang struct{}
	// Fail streams a JSON-RPC error as an event.
	Fail struct{ Err *RPCError }
	// Pause waits before the next step.
	Pause struct{ D time.Duration }
	// Comment streams an SSE comment line, which carries no event.
	Comment struct{}
)

func (Emit) step()    {}
func (Drop) step()    {}
func (Hang) step()    {}
func (Fail) step()    {}
func (Pause) step()   {}
func (Comment) step() {}

// Server is the fake. Create it with New. Its methods are safe for concurrent use.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	requests []Request
	faults   map[string][]Fault
	scripts  [][]Step
	agents   map[string]*api.Agent
	sessions map[string]*api.Session
	order    []string
	byReq    map[string]string                    // create request id -> session id
	first    map[string]*api.CreateSessionRequest // create request id -> the request that made its Session
	tasks    map[a2a.TaskID]*a2a.Task
	taskSeq  int
	sessSeq  int
	creates  []*api.CreateSessionRequest
	sched    scheduleStore

	// The Tamper hooks edit a reply, to test the client's checks that a reply belongs to its request.
	// Set them before the first request.
	TamperSession func(proc string, s *api.Session)
	TamperReceipt func(*api.WorkspacePreparationReceipt)
	// PrepareClassification is the classification the fake gives a prepared workspace's receipt;
	// empty means "confirmed". Use "pending" to keep a preparation open.
	PrepareClassification string
	// RequirePreparation makes a send to a Session with a workspace and a development environment
	// fail, as kagent does, until its workspace preparation is confirmed.
	RequirePreparation bool
	// Actors gives every READY Session a running Actor, and so a runtime association. Without it no
	// Session carries one.
	Actors bool
	// IdleActors, with Actors, parks a Session's Actor until it is resumed: an idle Session carries no
	// runtime association, and a workspace cannot be prepared.
	IdleActors  bool
	active      map[string]bool
	prepares    []*api.PrepareSessionWorkspaceRequest
	TamperAgent func(*api.Agent)
	// TamperSchedule edits a schedule in a ScheduledRunService reply, without changing the stored one.
	TamperSchedule func(proc string, sc *api.ScheduledRun)
	// TamperExecution edits the execution a TriggerScheduledRun reply carries.
	TamperExecution func(*api.ScheduledRunExecution)
	// TamperTask edits the task a CancelTask reply carries, without changing the stored task.
	TamperTask func(proc string, t *a2a.Task)
}

// New starts a fake kagent. Close it with Close.
func New() *Server {
	s := &Server{
		faults:   map[string][]Fault{},
		agents:   map[string]*api.Agent{},
		sessions: map[string]*api.Session{},
		byReq:    map[string]string{},
		first:    map[string]*api.CreateSessionRequest{},
		tasks:    map[a2a.TaskID]*a2a.Task{},
	}
	mux := http.NewServeMux()
	handle := func(proc string, h http.Handler) { mux.Handle(proc, s.guard(proc, h)) }
	handle(kagent.ProcCreateSession, connect.NewUnaryHandler(kagent.ProcCreateSession, s.createSession))
	handle(kagent.ProcPrepareSessionWorkspace,
		connect.NewUnaryHandler(kagent.ProcPrepareSessionWorkspace, s.prepare))
	handle(kagent.ProcGetSession, connect.NewUnaryHandler(kagent.ProcGetSession, s.getSession))
	handle(kagent.ProcListSessions, connect.NewUnaryHandler(kagent.ProcListSessions, s.listSessions))
	handle(kagent.ProcSuspendSession, connect.NewUnaryHandler(kagent.ProcSuspendSession, s.suspend))
	handle(kagent.ProcResumeSession, connect.NewUnaryHandler(kagent.ProcResumeSession, s.resume))
	handle(kagent.ProcDeleteSession, connect.NewUnaryHandler(kagent.ProcDeleteSession, s.deleteSession))
	handle(kagent.ProcGetAgent, connect.NewUnaryHandler(kagent.ProcGetAgent, s.getAgent))
	s.registerSchedules(handle)
	mux.HandleFunc("/agents/{ns}/{name}", s.a2a)
	s.Server = httptest.NewServer(mux)
	return s
}

// FailNext queues a fault for the next call with this key (a procedure or a JSON-RPC method).
func (s *Server) FailNext(key string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[key] = append(s.faults[key], f)
}

// Script queues the reply for the next SendStreamingMessage. With nothing queued, a send
// streams a submitted task, a working update and a completed task.
func (s *Server) Script(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts = append(s.scripts, steps)
}

// AddAgent makes GetAgent find this agent, with a prepared revision so Sessions can be created on it.
func (s *Server) AddAgent(namespace, name string) {
	s.setAgent(namespace, name, true)
}

// AddAgentNotReady adds an Agent whose first revision has not been prepared: GetAgent finds it and
// CreateSession refuses it, as kagent does.
func (s *Server) AddAgentNotReady(namespace, name string) {
	s.setAgent(namespace, name, false)
}

// RemoveAgent deletes the Agent: GetAgent no longer finds it and creates on it are refused.
func (s *Server) RemoveAgent(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, namespace+"/"+name)
}

func (s *Server) setAgent(namespace, name string, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := map[string]any{}
	if ready {
		status["latestSuccessfulRevision"] = "revision-1"
	}
	v, err := structpb.NewStruct(map[string]any{"status": status})
	if err != nil {
		panic(err)
	}
	s.agents[namespace+"/"+name] = &api.Agent{
		Ref:      &api.ResourceReference{Namespace: namespace, Name: name},
		Resource: &api.StructuredObject{Value: v},
	}
}

// AddTask makes GetTask and ListTasks find this task.
func (s *Server) AddTask(t *a2a.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

// DropTask forgets a task, so GetTask answers task not found: a runtime that lost it.
func (s *Server) DropTask(id a2a.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
}

// Requests returns the requests received so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// PrepareRequests returns the workspace preparations kagent accepted, in order.
func (s *Server) PrepareRequests() []*api.PrepareSessionWorkspaceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*api.PrepareSessionWorkspaceRequest(nil), s.prepares...)
}

// Count returns how many requests had this key.
func (s *Server) Count(key string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Key == key {
			n++
		}
	}
	return n
}

// Session returns the stored Session, for assertions.
func (s *Server) Session(id string) (*api.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	return proto.Clone(sess).(*api.Session), true
}

func (s *Server) record(r Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
}

func (s *Server) takeFault(key string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.faults[key]
	if len(q) == 0 {
		return Fault{}, false
	}
	s.faults[key] = q[1:]
	return q[0], true
}

// applyTransport applies the faults that do not depend on the protocol. It reports whether the
// request is finished.
func applyTransport(w http.ResponseWriter, r *http.Request, f Fault) bool {
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-r.Context().Done():
			return true
		}
	}
	switch {
	case f.Drop:
		drop(w)
		return true
	case f.Status != 0:
		w.WriteHeader(f.Status)
		return true
	}
	return false
}

func drop(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// guard records a gRPC-web request and applies its fault.
func (s *Server) guard(proc string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := next
		s.record(Request{Key: proc, Header: r.Header.Clone()})
		if f, ok := s.takeFault(proc); ok {
			if applyTransport(w, r, f) {
				return
			}
			if f.GRPC != nil {
				h = grpcFailure(*f.GRPC, f.Message)
			}
			if f.Truncate {
				h = truncated(h)
			}
		}
		h.ServeHTTP(w, r)
	})
}

// truncated runs next, then replays its reply with the body cut in half.
func truncated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		body := rec.Body.Bytes()
		_, _ = w.Write(body[:len(body)/2])
	})
}

// grpcFailure answers any gRPC-web request with an error status.
func grpcFailure(code connect.Code, message string) http.Handler {
	if message == "" {
		message = "injected " + code.String()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.Header().Set("Grpc-Status", strconv.Itoa(int(code)))
		w.Header().Set("Grpc-Message", message)
		w.WriteHeader(http.StatusOK)
	})
}

// ---- SessionService and AgentService -----------------------------------------------------

func (s *Server) createSession(
	_ context.Context, req *connect.Request[api.CreateSessionRequest],
) (*connect.Response[api.CreateSessionResponse], error) {
	m := req.Msg
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, proto.CloneOf(m))
	if id, ok := s.byReq[m.GetRequestId()]; ok && m.GetRequestId() != "" {
		// kagent compares the replay with the request that made the Session first, deleted or not.
		if !sameContract(s.first[m.GetRequestId()], m) {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("request id %q was used for a different session", m.GetRequestId()))
		}
		prev, live := s.sessions[id]
		if !live {
			// kagent keeps the request id of a deleted Session and refuses to reuse it.
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("request_id %q belongs to a deleted Session", m.GetRequestId()))
		}
		return connect.NewResponse(&api.CreateSessionResponse{Session: s.out(kagent.ProcCreateSession, prev)}), nil
	}
	// kagent looks the Agent up after the replay check and before it reserves the request id, so a
	// create it refuses here leaves nothing behind. A request that names no Agent
	// is let through, which kagent would refuse: the client tests create bare Sessions.
	if ref := m.GetAgent(); ref != nil && !kagent.AgentReady(s.agents[ref.GetNamespace()+"/"+ref.GetName()]) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("Agent does not have a ready prepared revision")) //nolint:staticcheck // kagent's own wording
	}
	s.sessSeq++
	id := fmt.Sprintf("session-%d", s.sessSeq)
	sess := &api.Session{
		Id: id, ContextId: id, Creator: "mainloop", Name: m.GetName(),
		Agent: proto.CloneOf(m.GetAgent()), Workspace: proto.CloneOf(m.GetWorkspace()),
		DevelopmentEnvironment: proto.CloneOf(m.GetDevelopmentEnvironment()),
		State:                  api.RuntimeState_RUNTIME_STATE_READY,
		CreatedAt:              timestamppb.Now(), UpdatedAt: timestamppb.Now(),
	}
	s.sessions[id] = sess
	s.order = append(s.order, id)
	s.byReq[m.GetRequestId()] = id
	s.first[m.GetRequestId()] = proto.CloneOf(m)
	return connect.NewResponse(&api.CreateSessionResponse{Session: s.out(kagent.ProcCreateSession, sess)}), nil
}

func (s *Server) lookup(id string) (*api.Session, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", id))
	}
	return sess, nil
}

func (s *Server) getSession(
	_ context.Context, req *connect.Request[api.GetSessionRequest],
) (*connect.Response[api.GetSessionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.GetSessionResponse{Session: s.out(kagent.ProcGetSession, sess)}), nil
}

func (s *Server) listSessions(
	_ context.Context, req *connect.Request[api.ListSessionsRequest],
) (*connect.Response[api.ListSessionsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start, _ := strconv.Atoi(req.Msg.GetPage().GetPageToken())
	limit := int(req.Msg.GetPage().GetLimit())
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	end := min(start+limit, len(s.order))
	resp := &api.ListSessionsResponse{Page: &api.PageResponse{}}
	for _, id := range s.order[min(start, end):end] {
		resp.Sessions = append(resp.Sessions, s.out(kagent.ProcListSessions, s.sessions[id]))
	}
	if end < len(s.order) {
		resp.Page.NextPageToken = strconv.Itoa(end)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) suspend(
	_ context.Context, req *connect.Request[api.SuspendSessionRequest],
) (*connect.Response[api.SuspendSessionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	sess.State = api.RuntimeState_RUNTIME_STATE_SUSPENDED
	return connect.NewResponse(&api.SuspendSessionResponse{Session: s.out(kagent.ProcSuspendSession, sess)}), nil
}

func (s *Server) resume(
	_ context.Context, req *connect.Request[api.ResumeSessionRequest],
) (*connect.Response[api.ResumeSessionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	sess.State = api.RuntimeState_RUNTIME_STATE_READY
	if s.active == nil {
		s.active = map[string]bool{}
	}
	s.active[sess.GetId()] = true
	return connect.NewResponse(&api.ResumeSessionResponse{Session: s.out(kagent.ProcResumeSession, sess)}), nil
}

func (s *Server) deleteSession(
	_ context.Context, req *connect.Request[api.DeleteSessionRequest],
) (*connect.Response[api.DeleteSessionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := req.Msg.GetSessionId()
	sess, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	delete(s.sessions, id)
	s.order = removeString(s.order, id)
	return connect.NewResponse(&api.DeleteSessionResponse{Session: s.out(kagent.ProcDeleteSession, sess)}), nil
}

func removeString(list []string, v string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) prepare(
	_ context.Context, req *connect.Request[api.PrepareSessionWorkspaceRequest],
) (*connect.Response[api.PrepareSessionWorkspaceResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	if s.Actors && !s.actorRunning(sess.GetId()) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no running actor"))
	}
	if !s.gitCredentialsCountable(sess.GetId()) {
		// kagent's preparation check needs exactly one read and one push Git proxy credential, each
		// with header and secret key exactly "authorization"; any other count or spelling fails it, and
		// kagent answers with this generic refusal, whatever the Actor's state.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("Current prepared runtime identity is unavailable")) //nolint:staticcheck // kagent's own wording
	}
	receipt := &api.WorkspacePreparationReceipt{
		Original: proto.CloneOf(req.Msg), ContextId: req.Msg.GetSessionId(), Classification: "confirmed",
	}
	if s.PrepareClassification != "" {
		receipt.Classification = s.PrepareClassification
	}
	sess.WorkspacePreparation = proto.CloneOf(receipt)
	s.prepares = append(s.prepares, proto.CloneOf(req.Msg))
	if s.TamperReceipt != nil {
		s.TamperReceipt(receipt)
	}
	return connect.NewResponse(&api.PrepareSessionWorkspaceResponse{Receipt: receipt}), nil
}

// gitCredentialsCountable reports whether the Session was created with exactly one read and one
// push Git proxy credential, spelled the way kagent's preparation check counts them (header and
// secret key exactly "authorization"); s.mu is held. kagent hard-codes the proxy origins with
// the `mainloop` namespace; the fake takes any namespace so a test namespace works, but nothing else
// (no port, path or trailing slash).
func (s *Server) gitCredentialsCountable(sessionID string) bool {
	read, push := 0, 0
	for requestID, id := range s.byReq {
		if id != sessionID {
			continue
		}
		for _, c := range s.first[requestID].GetCredentials() {
			if c.GetHeader() != "authorization" || c.GetSecretRef().GetKey() != "authorization" {
				continue
			}
			switch {
			case proxyOrigin(c.GetOrigin(), "read"):
				read++
			case proxyOrigin(c.GetOrigin(), "push"):
				push++
			}
		}
	}
	return read == 1 && push == 1
}

func (s *Server) getAgent(
	_ context.Context, req *connect.Request[api.GetAgentRequest],
) (*connect.Response[api.GetAgentResponse], error) {
	ref := req.Msg.GetRef()
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, ok := s.agents[ref.GetNamespace()+"/"+ref.GetName()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("agent %s/%s not found", ref.GetNamespace(), ref.GetName()))
	}
	out := proto.CloneOf(agent)
	if s.TamperAgent != nil {
		s.TamperAgent(out)
	}
	return connect.NewResponse(&api.GetAgentResponse{Agent: out}), nil
}

// ---- A2A -----------------------------------------------------------------------------------

type rpcCall struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *Server) a2a(w http.ResponseWriter, r *http.Request) {
	var call rpcCall
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&call); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rec := Request{Key: call.Method, AgentPath: r.URL.Path, Header: r.Header.Clone()}
	var send a2a.SendMessageRequest
	if call.Method == "SendStreamingMessage" {
		if err := json.Unmarshal(call.Params, &send); err != nil || send.Message == nil {
			http.Error(w, "bad params", http.StatusBadRequest)
			return
		}
		rec.MessageID = send.Message.ID
		rec.Metadata, rec.Extensions = send.Message.Metadata, send.Message.Extensions
	}
	s.record(rec)

	if f, ok := s.takeFault(call.Method); ok {
		if applyTransport(w, r, f) {
			return
		}
		if f.RPC != nil {
			writeEnvelope(w, call.ID, nil, f.RPC)
			return
		}
	}

	switch call.Method {
	case "SendStreamingMessage":
		s.send(w, r, call, send.Message)
	case "SubscribeToTask":
		var p a2a.SubscribeToTaskRequest
		_ = json.Unmarshal(call.Params, &p)
		s.subscribe(w, call, p.ID)
	case "GetTask":
		var p a2a.GetTaskRequest
		_ = json.Unmarshal(call.Params, &p)
		task, ok := s.task(p.ID)
		if !ok {
			writeEnvelope(w, call.ID, nil, taskNotFound())
			return
		}
		writeEnvelope(w, call.ID, task, nil)
	case "CancelTask":
		var p a2a.CancelTaskRequest
		_ = json.Unmarshal(call.Params, &p)
		task, ok := s.cancelTask(p.ID)
		if !ok {
			writeEnvelope(w, call.ID, nil, taskNotFound())
			return
		}
		if s.TamperTask != nil {
			s.TamperTask("CancelTask", task)
		}
		writeEnvelope(w, call.ID, task, nil)
	case "ListTasks":
		var p a2a.ListTasksRequest
		_ = json.Unmarshal(call.Params, &p)
		writeEnvelope(w, call.ID, s.listTasks(p), nil)
	default:
		writeEnvelope(w, call.ID, nil, &RPCError{Code: -32601, Message: "method not found"})
	}
}

func taskNotFound() *RPCError {
	return &RPCError{Code: -32001, Message: "task not found", Reason: "TASK_NOT_FOUND", Domain: "a2a-protocol.org"}
}

func (s *Server) task(id a2a.TaskID) (*a2a.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// cancelTask ends a task that has not ended, as kagent does; an ended task is returned unchanged.
func (s *Server) cancelTask(id a2a.TaskID) (*a2a.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	if !t.Status.State.Terminal() {
		cp := *t
		cp.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
		s.tasks[id] = &cp
		t = &cp
	}
	cp := *t
	return &cp, true
}

// Task returns the stored task, for assertions.
func (s *Server) Task(id a2a.TaskID) (*a2a.Task, bool) { return s.task(id) }

func (s *Server) listTasks(p a2a.ListTasksRequest) *a2a.ListTasksResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	var match []*a2a.Task
	for _, t := range s.tasks {
		if (p.ContextID == "" || t.ContextID == p.ContextID) && (p.Status == "" || t.Status.State == p.Status) {
			cp := *t
			if !p.IncludeArtifacts {
				cp.Artifacts = nil // a listed task has no artifacts unless asked for
			}
			if p.HistoryLength != nil && *p.HistoryLength < len(cp.History) {
				cp.History = cp.History[len(cp.History)-*p.HistoryLength:] // the most recent messages
			}
			match = append(match, &cp)
		}
	}
	slices.SortFunc(match, func(a, b *a2a.Task) int { return strings.Compare(string(a.ID), string(b.ID)) })
	start, _ := strconv.Atoi(p.PageToken)
	size := p.PageSize
	if size <= 0 || size > 100 {
		size = 50
	}
	end := min(start+size, len(match))
	resp := &a2a.ListTasksResponse{Tasks: match[min(start, end):end], TotalSize: len(match), PageSize: size}
	if end < len(match) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	return resp
}

// send streams the next script, or the default turn.
func (s *Server) send(w http.ResponseWriter, r *http.Request, call rpcCall, msg *a2a.Message) {
	s.mu.Lock()
	if sess := s.sessions[msg.ContextID]; s.RequirePreparation && sess.GetWorkspace() != nil &&
		sess.GetDevelopmentEnvironment() != nil && sess.GetWorkspacePreparation().GetClassification() != "confirmed" {
		s.mu.Unlock()
		writeEnvelope(w, call.ID, nil, &RPCError{Code: -32600, Message: "reply does not match the pending input request"})
		return
	}
	var steps []Step
	scripted := len(s.scripts) > 0
	if scripted {
		steps, s.scripts = s.scripts[0], s.scripts[1:]
	}
	s.mu.Unlock()
	if !scripted {
		steps = s.defaultTurn(msg)
	}
	s.stream(w, r, call, steps)
}

// defaultTurn records the message as a task and streams submitted, working and completed.
func (s *Server) defaultTurn(msg *a2a.Message) []Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := msg.TaskID
	if id == "" {
		s.taskSeq++
		id = a2a.TaskID(fmt.Sprintf("task-%d", s.taskSeq))
	}
	user := *msg
	user.TaskID = id
	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("ok"))
	reply.TaskID, reply.ContextID = id, msg.ContextID
	submitted := &a2a.Task{
		ID: id, ContextID: msg.ContextID, History: []*a2a.Message{&user},
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	done := &a2a.Task{
		ID: id, ContextID: msg.ContextID, History: []*a2a.Message{&user, reply},
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
	s.tasks[id] = done
	return []Step{
		Emit{submitted},
		Emit{a2a.NewStatusUpdateEvent(submitted, a2a.TaskStateWorking, nil)},
		Emit{done},
	}
}

func (s *Server) subscribe(w http.ResponseWriter, call rpcCall, id a2a.TaskID) {
	task, ok := s.task(id)
	if !ok {
		writeEnvelope(w, call.ID, nil, taskNotFound())
		return
	}
	s.stream(w, nil, call, []Step{Emit{task}})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, call rpcCall, steps []Step) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	for _, st := range steps {
		switch st := st.(type) {
		case Emit:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", envelope(call.ID, a2a.StreamResponse{Event: st.Event}, nil))
			_ = rc.Flush()
		case Pause:
			select {
			case <-time.After(st.D):
			case <-ctxDone(r):
				return
			}
		case Comment:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			_ = rc.Flush()
		case Fail:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", envelope(call.ID, nil, st.Err))
			_ = rc.Flush()
		case Drop:
			drop(w)
			return
		case Hang:
			if r != nil {
				<-r.Context().Done()
			}
			return
		}
	}
}

func writeEnvelope(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *RPCError) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(envelope(id, result, rpcErr))
}

func envelope(id json.RawMessage, result any, rpcErr *RPCError) []byte {
	env := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		e := map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
		if rpcErr.Reason != "" || rpcErr.Domain != "" {
			info := map[string]any{
				"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
				"reason": rpcErr.Reason, "domain": rpcErr.Domain,
			}
			if len(rpcErr.Metadata) > 0 {
				info["metadata"] = rpcErr.Metadata
			}
			e["data"] = []any{info}
		}
		env["error"] = e
	} else {
		env["result"] = result
	}
	body, err := json.Marshal(env)
	if err != nil {
		panic(err) // the fake builds only marshalable values
	}
	return body
}

// AgentPath is the A2A path of an agent, for assertions.
func AgentPath(namespace, name string) string {
	return "/agents/" + strings.Trim(namespace, "/") + "/" + name
}

// out copies a stored Session for a reply, so a reply is never marshalled while another request
// changes the stored one, and applies the tamper hook. The caller holds s.mu.
func (s *Server) out(proc string, sess *api.Session) *api.Session {
	cp := proto.CloneOf(sess)
	if s.Actors && sess.GetState() == api.RuntimeState_RUNTIME_STATE_READY {
		// A prepared Session is settled: no operation is open, and the runtime is pinned.
		cp.Operation = api.RuntimeOperation_RUNTIME_OPERATION_NONE
		cp.PreparedRevision = "revision-" + sess.GetId()
		cp.RuntimeComposition = &api.RuntimeComposition{PayloadImage: "example.test/harness@sha256:def", Provider: "claude", Schema: 1}
	}
	if sess.GetState() == api.RuntimeState_RUNTIME_STATE_READY && s.actorRunning(sess.GetId()) {
		cp.RuntimeAssociation = &api.RuntimeAssociation{
			GenerationId: "gen-" + sess.GetId(), Atespace: "ate", ActorName: "actor-" + sess.GetId(),
			ActorUid: "uid-" + sess.GetId(), Phase: "active", CurrentActive: true,
		}
	}
	if s.TamperSession != nil {
		s.TamperSession(proc, cp)
	}
	return cp
}

// actorRunning reports whether the Session's Actor is running; s.mu is held.
func (s *Server) actorRunning(id string) bool { return s.Actors && (!s.IdleActors || s.active[id]) }

// sameContract reports whether a replayed create matches the request that made the Session for the
// same request id: the Agent, the workspace, the environment and the credentials all have to match,
// as in kagent.
func sameContract(prev, m *api.CreateSessionRequest) bool {
	return proto.Equal(prev.GetAgent(), m.GetAgent()) &&
		proto.Equal(prev.GetWorkspace(), m.GetWorkspace()) &&
		proto.Equal(prev.GetDevelopmentEnvironment(), m.GetDevelopmentEnvironment()) &&
		slices.EqualFunc(prev.GetCredentials(), m.GetCredentials(), func(a, b *api.SessionCredential) bool { return proto.Equal(a, b) })
}

// ctxDone is r's done channel, or a channel that never closes when there is no request.
func ctxDone(r *http.Request) <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.Context().Done()
}

// CreateRequests returns a copy of every CreateSession request the fake has received, in order,
// including replays and refused ones.
func (s *Server) CreateRequests() []*api.CreateSessionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*api.CreateSessionRequest, len(s.creates))
	for i, m := range s.creates {
		out[i] = proto.CloneOf(m)
	}
	return out
}

// proxyOrigin reports whether origin is exactly http://mainloop-git-<service>.<namespace>.svc.cluster.local.
func proxyOrigin(origin, service string) bool {
	ns, ok := strings.CutPrefix(origin, "http://mainloop-git-"+service+".")
	if !ok {
		return false
	}
	ns, ok = strings.CutSuffix(ns, ".svc.cluster.local")
	return ok && ns != "" && !strings.ContainsAny(ns, "./:")
}
