package config

import "strings"

// Sessions tunes the native sessions' delivery ledger.
type Sessions struct {
	// LiveAgents names the kagent Agents (by name) whose Harness runs the Agent SDK runner
	// (KAGENT_CLAUDE_DRIVER=sdk). Their sessions get GET /sessions/{id}/live.
	LiveAgents []string `env:"MAINLOOP_LIVE_AGENTS" envSeparator:","`
	// SteerAgents names the kagent Agents (by name) whose harness takes a message into a running
	// turn (A2A SendMessage to the running task with the kagent.steer flag). An owner's message
	// sent with steer to one of their sessions joins the running turn instead of queueing behind it.
	// Any other Agent's steer is an ordinary message.
	SteerAgents []string `env:"MAINLOOP_STEER_AGENTS" envSeparator:","`
	// UncertainHold is how long a delivery whose send may have reached kagent keeps blocking new
	// sends while it is looked up. After it, a message that no kagent task shows is marked failed.
	UncertainHold Seconds `env:"SESSION_UNCERTAIN_HOLD_SECONDS" envDefault:"1800"`
	// UncertainRecheck is how often an unresolved delivery is looked up again.
	UncertainRecheck Seconds `env:"SESSION_UNCERTAIN_RECHECK_SECONDS" envDefault:"30"`
	// FollowPoll is how often a running task is read again once its event stream has ended.
	FollowPoll Seconds `env:"SESSION_FOLLOW_POLL_SECONDS" envDefault:"2"`
	// ParkedMax is the longest wait between reads of a task parked for an answer (input or auth
	// required). The wait grows from FollowPoll up to this.
	ParkedMax Seconds `env:"SESSION_PARKED_MAX_SECONDS" envDefault:"300"`
	// ReadyWait is how long a send waits for a starting or resuming kagent Session to be ready.
	ReadyWait Seconds `env:"SESSION_READY_WAIT_SECONDS" envDefault:"60"`
	// DeliveryConcurrency is how many deliveries are handled at once. A handler follows a turn to
	// its end, so this is the number of turns that can run at the same time.
	DeliveryConcurrency int `env:"SESSION_DELIVERY_CONCURRENCY" envDefault:"16"`
	// EnsureEvery is how often the main session is checked for.
	EnsureEvery Seconds `env:"SESSION_ENSURE_SECONDS" envDefault:"60"`
	// CarryOverMessages is how many recent completed user and assistant messages the first
	// message to a new main Session carries from the earlier conversation. 0 carries none; at most 100.
	CarryOverMessages int `env:"MAIN_CARRY_OVER_MESSAGES" envDefault:"6"`
	// StallAfter is how long a delivered turn may go without a task event before it reads as
	// stalled. Generous: a turn that thinks for a long time says nothing meanwhile.
	StallAfter Seconds `env:"SESSION_STALL_SECONDS" envDefault:"1500"`
	// StallToolAfter replaces StallAfter while the last event started a tool call with no result yet:
	// a web search or a long build emits nothing until it ends.
	StallToolAfter Seconds `env:"SESSION_STALL_TOOL_SECONDS" envDefault:"3600"`
}

// IsLiveAgent reports whether the named Agent runs the Agent SDK runner.
func (s Sessions) IsLiveAgent(name string) bool { return listed(s.LiveAgents, name) }

// IsSteerAgent reports whether the named Agent takes a message into a running turn.
func (s Sessions) IsSteerAgent(name string) bool { return listed(s.SteerAgents, name) }

func listed(names []string, name string) bool {
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && n == name {
			return true
		}
	}
	return false
}
