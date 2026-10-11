// Package schedules serves scheduled agent runs. kagent owns schedules and their executions; this
// package proxies the /schedules routes to it and turns each new execution into a Mainloop session
// and, when it fails, an inbox item.
package schedules

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/providers"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Kagent is the part of the kagent client the schedules use.
type Kagent interface {
	CreateScheduledRun(ctx context.Context, req *kagentapi.CreateScheduledRunRequest) (*kagentapi.ScheduledRun, error)
	GetScheduledRun(ctx context.Context, id string) (*kagentapi.ScheduledRun, error)
	ListScheduledRunsPage(ctx context.Context, token string, limit int) ([]*kagentapi.ScheduledRun, string, error)
	ListScheduledRuns(ctx context.Context) ([]*kagentapi.ScheduledRun, error)
	UpdateScheduledRun(ctx context.Context, id, etag string, config *kagentapi.ScheduledRunConfig) (*kagentapi.ScheduledRun, error)
	DeleteScheduledRun(ctx context.Context, id string) (*kagentapi.ScheduledRun, error)
	TriggerScheduledRun(ctx context.Context, id, requestID string) (*kagentapi.ScheduledRunExecution, error)
	ListScheduledRunExecutionsPage(
		ctx context.Context, scheduleID, token string, limit int,
	) ([]*kagentapi.ScheduledRunExecution, string, error)
}

var _ Kagent = (*kagent.Client)(nil)

// Service is the schedules domain.
type Service struct {
	Pool *pgxpool.Pool
	// Kagent is nil in processes that do not reach kagent; the routes then answer 503 and the sweep
	// does nothing.
	Kagent Kagent
	// Providers picks the binding of an adopted session from the schedule's Agent. May be nil.
	Providers *providers.Registry
	OwnerID   string
	Log       *slog.Logger
	// SweepEvery is the adoption sweep's period; zero means 5 seconds.
	SweepEvery time.Duration
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

const (
	defaultTimeout = 15 * time.Minute
	maxPromptBytes = 32768
)

// toConfig turns the contract's config into kagent's. An omitted timeout stays unset, so kagent
// applies its own default.
func toConfig(in api.ScheduledRunConfigInput) (*kagentapi.ScheduledRunConfig, error) {
	if len(in.Prompt) > maxPromptBytes {
		return nil, fmt.Errorf("prompt is longer than %d bytes", maxPromptBytes)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt is blank")
	}
	c := &kagentapi.ScheduledRunConfig{Schedule: in.Schedule, Prompt: in.Prompt}
	if in.Name != nil {
		c.Name = *in.Name
	}
	if in.TimeZone != nil {
		c.TimeZone = *in.TimeZone
	}
	if in.Paused != nil {
		c.Paused = *in.Paused
	}
	if in.ExecutionTimeoutSeconds != nil {
		secs := *in.ExecutionTimeoutSeconds
		if secs < 1e-6 || secs > 9223372036 {
			return nil, fmt.Errorf("execution_timeout_seconds is out of range")
		}
		c.ExecutionTimeout = durationpb.New(time.Duration(secs * float64(time.Second)))
	}
	return c, nil
}

func fromSchedule(s *kagentapi.ScheduledRun) api.ScheduledRunOutput {
	timeout := defaultTimeout
	if d := s.GetConfig().GetExecutionTimeout(); d != nil {
		timeout = d.AsDuration()
	}
	out := api.ScheduledRunOutput{
		Id:    s.GetId(),
		Agent: api.ScheduledRunAgentRef{Namespace: s.GetAgent().GetNamespace(), Name: s.GetAgent().GetName()},
		Config: api.ScheduledRunConfigOutput{
			Name: s.GetConfig().GetName(), Schedule: s.GetConfig().GetSchedule(),
			TimeZone: s.GetConfig().GetTimeZone(), Prompt: s.GetConfig().GetPrompt(),
			Paused: s.GetConfig().GetPaused(), ExecutionTimeoutSeconds: timeout.Seconds(),
		},
		Etag:      s.GetEtag(),
		CreatedAt: s.GetCreatedAt().AsTime(),
		UpdatedAt: s.GetUpdatedAt().AsTime(),
	}
	if t := s.GetNextExecutionTime(); t != nil {
		v := t.AsTime()
		out.NextExecutionTime = &v
	}
	return out
}

// Execution states, as the contract spells them.
const (
	statePending   = "pending"
	stateRunning   = "running"
	stateSucceeded = "succeeded"
	stateFailed    = "failed"
	stateTimedOut  = "timed_out"
)

func stateName(s kagentapi.ScheduledRunExecutionState) string {
	switch s {
	case kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING:
		return stateRunning
	case kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_SUCCEEDED:
		return stateSucceeded
	case kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_FAILED:
		return stateFailed
	case kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_TIMED_OUT:
		return stateTimedOut
	}
	return statePending
}

func terminal(state string) bool {
	return state == stateSucceeded || state == stateFailed || state == stateTimedOut
}

func failed(state string) bool { return state == stateFailed || state == stateTimedOut }

// fromExecution converts an execution. sessionID is the Mainloop session the execution became, or
// nil while it has none; kagent's own session id is not the contract's session id.
func fromExecution(e *kagentapi.ScheduledRunExecution, sessionID *string) api.ScheduledRunExecutionOutput {
	out := api.ScheduledRunExecutionOutput{
		Id: e.GetId(), ScheduledRunId: e.GetScheduledRunId(), Prompt: e.GetPrompt(),
		CreatedAt: e.GetCreatedAt().AsTime(), Deadline: e.GetDeadline().AsTime(),
		SessionId: sessionID, State: api.ScheduledRunExecutionOutputState(stateName(e.GetState())),
	}
	if t := e.GetScheduledTime(); t != nil {
		v := t.AsTime()
		out.ScheduledTime = &v
	}
	if id := e.GetManualRequestId(); id != "" {
		out.ManualRequestId = &id
	}
	if id := e.GetTaskId(); id != "" {
		out.TaskId = &id
	}
	if t := e.GetCompletedAt(); t != nil {
		v := t.AsTime()
		out.CompletedAt = &v
	}
	if r := e.GetFailureReason(); r != "" {
		out.FailureReason = &r
	}
	return out
}
