package kagenttest

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/kagent"
	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// checkpointStore is the CheckpointService state. The zero value is ready to use; its fields are
// guarded by Server.mu.
type checkpointStore struct {
	checkpoints map[string]*api.Checkpoint // by id
	order       []string
	byReq       map[string]string                       // CreateCheckpoint request id -> checkpoint id
	first       map[string]*api.CreateCheckpointRequest // CreateCheckpoint request id -> its first request
	forkByReq   map[string]string                       // ForkSession request id -> forked session id
	forkFirst   map[string]*api.ForkSessionRequest      // ForkSession request id -> its first request
	seq         int
}

func (st *checkpointStore) init() {
	if st.checkpoints == nil {
		st.checkpoints = map[string]*api.Checkpoint{}
		st.byReq = map[string]string{}
		st.first = map[string]*api.CreateCheckpointRequest{}
		st.forkByReq = map[string]string{}
		st.forkFirst = map[string]*api.ForkSessionRequest{}
	}
}

func (s *Server) registerCheckpoints(handle func(string, http.Handler)) {
	handle(kagent.ProcCreateCheckpoint, connect.NewUnaryHandler(kagent.ProcCreateCheckpoint, s.createCheckpoint))
	handle(kagent.ProcGetCheckpoint, connect.NewUnaryHandler(kagent.ProcGetCheckpoint, s.getCheckpoint))
	handle(kagent.ProcListCheckpoints, connect.NewUnaryHandler(kagent.ProcListCheckpoints, s.listCheckpoints))
	handle(kagent.ProcDeleteCheckpoint, connect.NewUnaryHandler(kagent.ProcDeleteCheckpoint, s.deleteCheckpoint))
	handle(kagent.ProcForkSession, connect.NewUnaryHandler(kagent.ProcForkSession, s.forkSession))
}

func (s *Server) createCheckpoint(
	_ context.Context, req *connect.Request[api.CreateCheckpointRequest],
) (*connect.Response[api.CreateCheckpointResponse], error) {
	m := req.Msg
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.cp
	st.init()
	if id, ok := st.byReq[m.GetRequestId()]; ok {
		prev := st.first[m.GetRequestId()]
		if prev.GetSessionId() != m.GetSessionId() || prev.GetExpectedHeadTaskId() != m.GetExpectedHeadTaskId() {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("request id %q was already used for a different checkpoint", m.GetRequestId()))
		}
		return connect.NewResponse(&api.CreateCheckpointResponse{Checkpoint: proto.CloneOf(st.checkpoints[id])}), nil
	}
	sess, ok := s.sessions[m.GetSessionId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", m.GetSessionId()))
	}
	task, ok := s.tasks[a2a.TaskID(m.GetExpectedHeadTaskId())]
	if !ok || !task.Status.State.Terminal() {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("task %q is not the session's terminal task", m.GetExpectedHeadTaskId()))
	}
	st.seq++
	cp := &api.Checkpoint{
		Id: fmt.Sprintf("checkpoint-%d", st.seq), SessionId: m.GetSessionId(), HeadTaskId: m.GetExpectedHeadTaskId(),
		State: api.CheckpointState_CHECKPOINT_STATE_READY, CreatedAt: timestamppb.Now(),
		Name: fmt.Sprintf("%s at %s", sess.GetName(), m.GetExpectedHeadTaskId()),
	}
	st.checkpoints[cp.Id] = cp
	st.order = append(st.order, cp.Id)
	st.byReq[m.GetRequestId()] = cp.Id
	st.first[m.GetRequestId()] = proto.CloneOf(m)
	return connect.NewResponse(&api.CreateCheckpointResponse{Checkpoint: proto.CloneOf(cp)}), nil
}

func (s *Server) getCheckpoint(
	_ context.Context, req *connect.Request[api.GetCheckpointRequest],
) (*connect.Response[api.GetCheckpointResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.cp.checkpoints[req.Msg.GetCheckpointId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("checkpoint %q not found", req.Msg.GetCheckpointId()))
	}
	return connect.NewResponse(&api.GetCheckpointResponse{Checkpoint: proto.CloneOf(cp)}), nil
}

func (s *Server) listCheckpoints(
	_ context.Context, req *connect.Request[api.ListCheckpointsRequest],
) (*connect.Response[api.ListCheckpointsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.cp
	st.init()
	start, _ := strconv.Atoi(req.Msg.GetPage().GetPageToken())
	limit := int(req.Msg.GetPage().GetLimit())
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var matched []string
	for _, id := range st.order {
		if st.checkpoints[id].GetSessionId() == req.Msg.GetSessionId() {
			matched = append(matched, id)
		}
	}
	end := min(start+limit, len(matched))
	resp := &api.ListCheckpointsResponse{Page: &api.PageResponse{}}
	for _, id := range matched[min(start, end):end] {
		resp.Checkpoints = append(resp.Checkpoints, proto.CloneOf(st.checkpoints[id]))
	}
	if end < len(matched) {
		resp.Page.NextPageToken = strconv.Itoa(end)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) deleteCheckpoint(
	_ context.Context, req *connect.Request[api.DeleteCheckpointRequest],
) (*connect.Response[api.DeleteCheckpointResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := req.Msg.GetCheckpointId()
	if _, ok := s.cp.checkpoints[id]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("checkpoint %q not found", id))
	}
	delete(s.cp.checkpoints, id)
	s.cp.order = removeString(s.cp.order, id)
	return connect.NewResponse(&api.DeleteCheckpointResponse{}), nil
}

func (s *Server) forkSession(
	_ context.Context, req *connect.Request[api.ForkSessionRequest],
) (*connect.Response[api.ForkSessionResponse], error) {
	m := req.Msg
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.cp
	st.init()
	if id, ok := st.forkByReq[m.GetRequestId()]; ok {
		prev := st.forkFirst[m.GetRequestId()]
		if prev.GetCheckpointId() != m.GetCheckpointId() || prev.GetFreshConversation() != m.GetFreshConversation() {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("request id %q was already used for a different fork", m.GetRequestId()))
		}
		return connect.NewResponse(&api.ForkSessionResponse{Session: s.out(kagent.ProcForkSession, s.sessions[id])}), nil
	}
	cp, ok := st.checkpoints[m.GetCheckpointId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("checkpoint %q not found", m.GetCheckpointId()))
	}
	src, ok := s.sessions[cp.GetSessionId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", cp.GetSessionId()))
	}
	s.sessSeq++
	id := fmt.Sprintf("session-%d", s.sessSeq)
	forked := &api.Session{
		Id: id, ContextId: id, Creator: src.GetCreator(), Agent: proto.CloneOf(src.GetAgent()),
		Workspace: proto.CloneOf(src.GetWorkspace()), DevelopmentEnvironment: proto.CloneOf(src.GetDevelopmentEnvironment()),
		State: api.RuntimeState_RUNTIME_STATE_READY, CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(),
	}
	s.sessions[id] = forked
	s.order = append(s.order, id)
	st.forkByReq[m.GetRequestId()] = id
	st.forkFirst[m.GetRequestId()] = proto.CloneOf(m)
	return connect.NewResponse(&api.ForkSessionResponse{Session: s.out(kagent.ProcForkSession, forked)}), nil
}
