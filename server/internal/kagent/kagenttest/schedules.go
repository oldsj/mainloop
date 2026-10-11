package kagenttest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/kagent"
	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// scheduleStore is the ScheduledRunService state. The zero value is ready to use; its fields are
// guarded by Server.mu.
type scheduleStore struct {
	schedules  map[string]*api.ScheduledRun // by id, deleted ones included (tombstones)
	byReq      map[string]string            // create request id -> schedule id
	first      map[string]*api.CreateScheduledRunRequest
	executions map[string]*api.ScheduledRunExecution // by id
	creates    int
}

func (st *scheduleStore) init() {
	if st.schedules == nil {
		st.schedules = map[string]*api.ScheduledRun{}
		st.byReq = map[string]string{}
		st.first = map[string]*api.CreateScheduledRunRequest{}
		st.executions = map[string]*api.ScheduledRunExecution{}
	}
}

const defaultExecutionTimeout = 15 * time.Minute

func (s *Server) registerSchedules(handle func(string, http.Handler)) {
	handle(kagent.ProcCreateScheduledRun, connect.NewUnaryHandler(kagent.ProcCreateScheduledRun, s.createSchedule))
	handle(kagent.ProcGetScheduledRun, connect.NewUnaryHandler(kagent.ProcGetScheduledRun, s.getSchedule))
	handle(kagent.ProcListScheduledRuns, connect.NewUnaryHandler(kagent.ProcListScheduledRuns, s.listSchedules))
	handle(kagent.ProcUpdateScheduledRun, connect.NewUnaryHandler(kagent.ProcUpdateScheduledRun, s.updateSchedule))
	handle(kagent.ProcDeleteScheduledRun, connect.NewUnaryHandler(kagent.ProcDeleteScheduledRun, s.deleteSchedule))
	handle(kagent.ProcTriggerScheduledRun, connect.NewUnaryHandler(kagent.ProcTriggerScheduledRun, s.triggerSchedule))
	handle(kagent.ProcGetScheduledRunExecution,
		connect.NewUnaryHandler(kagent.ProcGetScheduledRunExecution, s.getExecution))
	handle(kagent.ProcListScheduledRunExecutions,
		connect.NewUnaryHandler(kagent.ProcListScheduledRunExecutions, s.listExecutions))
}

// normalize applies kagent's defaults: UTC, and a 15 minute execution timeout.
func normalize(c *api.ScheduledRunConfig) (*api.ScheduledRunConfig, error) {
	c = proto.CloneOf(c)
	if c.GetSchedule() == "" || len(strings.Fields(c.GetSchedule())) != 5 || strings.HasPrefix(c.GetSchedule(), "@") {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid schedule %q", c.GetSchedule()))
	}
	if c.GetPrompt() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("prompt is required"))
	}
	if c.GetTimeZone() == "" {
		c.TimeZone = "UTC"
	}
	if _, err := time.LoadLocation(c.GetTimeZone()); err != nil || c.GetTimeZone() == "Local" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid time zone %q", c.GetTimeZone()))
	}
	if c.GetExecutionTimeout() == nil {
		c.ExecutionTimeout = durationpb.New(defaultExecutionTimeout)
	}
	return c, nil
}

func nextTime(c *api.ScheduledRunConfig) *timestamppb.Timestamp {
	if c.GetPaused() {
		return nil
	}
	return timestamppb.New(time.Now().Add(time.Hour))
}

func notFound(what, id string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s %q not found", what, id))
}

func (s *Server) outSchedule(proc string, sc *api.ScheduledRun) *api.ScheduledRun {
	cp := proto.CloneOf(sc)
	if s.TamperSchedule != nil {
		s.TamperSchedule(proc, cp)
	}
	return cp
}

func (s *Server) createSchedule(
	_ context.Context, req *connect.Request[api.CreateScheduledRunRequest],
) (*connect.Response[api.CreateScheduledRunResponse], error) {
	m := req.Msg
	cfg, err := normalize(m.GetConfig())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.sched
	st.init()
	st.creates++
	// The replay is compared with the normalized inputs of the first request, and returns the
	// schedule as it is now, edited or deleted.
	if id, ok := st.byReq[m.GetRequestId()]; ok {
		prev := st.first[m.GetRequestId()]
		if !proto.Equal(prev.GetAgent(), m.GetAgent()) || !proto.Equal(prev.GetConfig(), cfg) {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("request id %q was already used for different inputs", m.GetRequestId()))
		}
		return connect.NewResponse(&api.CreateScheduledRunResponse{
			ScheduledRun: s.outSchedule(kagent.ProcCreateScheduledRun, st.schedules[id]),
		}), nil
	}
	ref := m.GetAgent()
	if _, ok := s.agents[ref.GetNamespace()+"/"+ref.GetName()]; !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the Agent does not exist"))
	}
	now := timestamppb.Now()
	sc := &api.ScheduledRun{
		Id: uuid.NewString(), Creator: "mainloop", Agent: proto.CloneOf(ref), Config: cfg,
		Etag: uuid.NewString(), NextExecutionTime: nextTime(cfg), CreatedAt: now, UpdatedAt: now,
	}
	st.schedules[sc.Id] = sc
	st.byReq[m.GetRequestId()] = sc.Id
	st.first[m.GetRequestId()] = &api.CreateScheduledRunRequest{Agent: proto.CloneOf(ref), Config: proto.CloneOf(cfg)}
	return connect.NewResponse(&api.CreateScheduledRunResponse{
		ScheduledRun: s.outSchedule(kagent.ProcCreateScheduledRun, sc),
	}), nil
}

func (s *Server) schedule(id string) (*api.ScheduledRun, error) {
	sc, ok := s.sched.schedules[id]
	if !ok {
		return nil, notFound("scheduled run", id)
	}
	return sc, nil
}

func (s *Server) getSchedule(
	_ context.Context, req *connect.Request[api.GetScheduledRunRequest],
) (*connect.Response[api.GetScheduledRunResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	sc, err := s.schedule(req.Msg.GetScheduledRunId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&api.GetScheduledRunResponse{
		ScheduledRun: s.outSchedule(kagent.ProcGetScheduledRun, sc),
	}), nil
}

// pageLimit is the effective page size: the default is 50, and 100 is the most.
func pageLimit(p *api.PageRequest) (int, error) {
	n := int(p.GetLimit())
	switch {
	case n == 0:
		return 50, nil
	case n < 1 || n > 100:
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid page size"))
	}
	return n, nil
}

func (s *Server) listSchedules(
	_ context.Context, req *connect.Request[api.ListScheduledRunsRequest],
) (*connect.Response[api.ListScheduledRunsResponse], error) {
	limit, err := pageLimit(req.Msg.GetPage())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	after := req.Msg.GetPage().GetPageToken()
	var ids []string
	for id, sc := range s.sched.schedules {
		if sc.GetDeletedAt() == nil && id > after {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	resp := &api.ListScheduledRunsResponse{Page: &api.PageResponse{}}
	if len(ids) > limit {
		ids = ids[:limit]
		resp.Page.NextPageToken = ids[limit-1]
	}
	for _, id := range ids {
		resp.ScheduledRuns = append(resp.ScheduledRuns, s.outSchedule(kagent.ProcListScheduledRuns, s.sched.schedules[id]))
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) updateSchedule(
	_ context.Context, req *connect.Request[api.UpdateScheduledRunRequest],
) (*connect.Response[api.UpdateScheduledRunResponse], error) {
	m := req.Msg
	cfg, err := normalize(m.GetConfig())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	sc, err := s.schedule(m.GetScheduledRunId())
	if err != nil {
		return nil, err
	}
	if sc.GetDeletedAt() != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("ScheduledRun %s was deleted", sc.GetId()))
	}
	if sc.GetEtag() != m.GetEtag() {
		return nil, connect.NewError(connect.CodeAborted,
			fmt.Errorf("ScheduledRun %s changed; reload before updating", sc.GetId()))
	}
	sc.Config, sc.Etag, sc.UpdatedAt = cfg, uuid.NewString(), timestamppb.Now()
	sc.NextExecutionTime = nextTime(cfg)
	return connect.NewResponse(&api.UpdateScheduledRunResponse{
		ScheduledRun: s.outSchedule(kagent.ProcUpdateScheduledRun, sc),
	}), nil
}

func (s *Server) deleteSchedule(
	_ context.Context, req *connect.Request[api.DeleteScheduledRunRequest],
) (*connect.Response[api.DeleteScheduledRunResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	sc, err := s.schedule(req.Msg.GetScheduledRunId())
	if err != nil {
		return nil, err
	}
	if sc.GetDeletedAt() == nil {
		sc.DeletedAt, sc.NextExecutionTime, sc.UpdatedAt = timestamppb.Now(), nil, timestamppb.Now()
	}
	return connect.NewResponse(&api.DeleteScheduledRunResponse{
		ScheduledRun: s.outSchedule(kagent.ProcDeleteScheduledRun, sc),
	}), nil
}

func (s *Server) newExecution(sc *api.ScheduledRun) *api.ScheduledRunExecution {
	id, _ := uuid.NewV7()
	now := time.Now()
	e := &api.ScheduledRunExecution{
		Id: id.String(), Creator: sc.GetCreator(), ScheduledRunId: sc.GetId(), Prompt: sc.GetConfig().GetPrompt(),
		CreatedAt: timestamppb.New(now),
		Deadline:  timestamppb.New(now.Add(sc.GetConfig().GetExecutionTimeout().AsDuration())),
		State:     api.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_PENDING,
	}
	s.sched.executions[e.Id] = e
	return e
}

func (s *Server) triggerSchedule(
	_ context.Context, req *connect.Request[api.TriggerScheduledRunRequest],
) (*connect.Response[api.TriggerScheduledRunResponse], error) {
	m := req.Msg
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	sc, err := s.schedule(m.GetScheduledRunId())
	if err != nil {
		return nil, err
	}
	if m.GetRequestId() == "" || len(m.GetRequestId()) > 128 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid request id"))
	}
	// A repeated request id returns its execution, even after the schedule was deleted.
	for _, e := range s.sched.executions {
		if e.GetScheduledRunId() == sc.GetId() && e.GetManualRequestId() == m.GetRequestId() {
			return connect.NewResponse(&api.TriggerScheduledRunResponse{Execution: s.outExecution(e)}), nil
		}
	}
	if sc.GetDeletedAt() != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("ScheduledRun %s was deleted", sc.GetId()))
	}
	e := s.newExecution(sc)
	e.Trigger = &api.ScheduledRunExecution_ManualRequestId{ManualRequestId: m.GetRequestId()}
	return connect.NewResponse(&api.TriggerScheduledRunResponse{Execution: s.outExecution(e)}), nil
}

func (s *Server) outExecution(e *api.ScheduledRunExecution) *api.ScheduledRunExecution {
	cp := proto.CloneOf(e)
	if s.TamperExecution != nil {
		s.TamperExecution(cp)
	}
	return cp
}

func (s *Server) getExecution(
	_ context.Context, req *connect.Request[api.GetScheduledRunExecutionRequest],
) (*connect.Response[api.GetScheduledRunExecutionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	e, ok := s.sched.executions[req.Msg.GetExecutionId()]
	if !ok {
		return nil, notFound("execution", req.Msg.GetExecutionId())
	}
	return connect.NewResponse(&api.GetScheduledRunExecutionResponse{Execution: proto.CloneOf(e)}), nil
}

func (s *Server) listExecutions(
	_ context.Context, req *connect.Request[api.ListScheduledRunExecutionsRequest],
) (*connect.Response[api.ListScheduledRunExecutionsResponse], error) {
	limit, err := pageLimit(req.Msg.GetPage())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	if _, err := s.schedule(req.Msg.GetScheduledRunId()); err != nil {
		return nil, err
	}
	before := req.Msg.GetPage().GetPageToken()
	var ids []string
	for id, e := range s.sched.executions {
		if e.GetScheduledRunId() == req.Msg.GetScheduledRunId() && (before == "" || id < before) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	slices.Reverse(ids) // newest first
	resp := &api.ListScheduledRunExecutionsResponse{Page: &api.PageResponse{}}
	if len(ids) > limit {
		ids = ids[:limit]
		resp.Page.NextPageToken = ids[limit-1]
	}
	for _, id := range ids {
		resp.Executions = append(resp.Executions, proto.CloneOf(s.sched.executions[id]))
	}
	return connect.NewResponse(resp), nil
}

// ---- Test controls -------------------------------------------------------------------------

// ScheduledRun returns the stored schedule (a tombstone when deleted), for assertions.
func (s *Server) ScheduledRun(id string) (*api.ScheduledRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.sched.schedules[id]
	if !ok {
		return nil, false
	}
	return proto.CloneOf(sc), true
}

// ScheduleCreates counts the CreateScheduledRun calls that reached the handler.
func (s *Server) ScheduleCreates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sched.creates
}

// ScheduleCount is how many schedules exist, deleted ones included.
func (s *Server) ScheduleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sched.schedules)
}

// Fire adds a cron execution to a schedule, as kagent's scheduler would when the schedule is due.
// The execution starts pending with no session.
func (s *Server) Fire(scheduleID string, at time.Time) *api.ScheduledRunExecution {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sched.init()
	sc, ok := s.sched.schedules[scheduleID]
	if !ok {
		panic("kagenttest: Fire on an unknown schedule " + scheduleID)
	}
	e := s.newExecution(sc)
	e.Trigger = &api.ScheduledRunExecution_ScheduledTime{ScheduledTime: timestamppb.New(at)}
	return proto.CloneOf(e)
}

// EditExecution changes a stored execution, as the runtime would when it reserves a session or
// finishes. It returns the execution after the edit.
func (s *Server) EditExecution(id string, edit func(*api.ScheduledRunExecution)) *api.ScheduledRunExecution {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sched.executions[id]
	if !ok {
		panic("kagenttest: EditExecution on an unknown execution " + id)
	}
	edit(e)
	return proto.CloneOf(e)
}
