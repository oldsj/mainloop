package hitl

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/oldsj/mainloop/server/internal/kagent"
)

// turnCap bounds how long the stream of a sent response is read after SendHITL has returned. The
// response resumes the task, so the stream carries the rest of the agent's turn; cutting it off
// early could end that turn.
const turnCap = 30 * time.Minute

// abortWait bounds how long SendHITL waits for the background send to report after it was stopped.
// The send honours its context, so it reports at once; the bound only keeps a stuck transport from
// holding the caller.
const abortWait = 5 * time.Second

// responseText is the text part of a HITL response message. kagent reads the answer from the
// extension metadata; the text only has to be non-empty.
const responseText = "HITL response"

// FromClient adapts the shared kagent client.
func FromClient(c *kagent.Client) Kagent { return clientAdapter{c} }

type clientAdapter struct{ *kagent.Client }

// SendHITL sends the response as one user message to the waiting task, with the answer in the
// HITL extension metadata and the extension named on the message (the client also puts it in the
// A2A-Extensions header of every call). It returns when kagent first reports the task, when the
// send fails, or when ctx ends; after the first report the stream is read on in the background so
// the agent's turn is not cut short. Errors keep their kagent types, so kagent.NotSent still tells
// a send that never left from one that may have.
//
// When ctx ends before kagent has reported the task, the send is stopped before SendHITL returns,
// and the error is what the stopped send says: a refusal that was only being retried is still
// NotSent, and a request that may have reached kagent is not. So "not sent" is never reported while
// a retry is still running, and a later delivery cannot follow it.
func (a clientAdapter) SendHITL(ctx context.Context, agent kagent.AgentRef, s Send) error {
	if err := ctx.Err(); err != nil {
		return &kagent.Unreachable{Err: err} // nothing was sent
	}
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnCap)
	first := make(chan error, 1) // receives exactly one value
	var (
		mu      sync.Mutex
		seen    bool // kagent reported the task
		aborted bool // the caller stopped waiting
	)
	go func() {
		defer cancel()
		reported := false
		for ev, err := range a.SendMessage(run, agent, kagent.SendParams{
			Text: responseText, MessageID: s.MessageID, ContextID: s.ContextID, TaskID: s.TaskID,
			Metadata:   map[string]any{kagent.HITLExtension: s.Native},
			Extensions: []string{kagent.HITLExtension},
		}) {
			if err != nil {
				if !reported {
					first <- err
				}
				return
			}
			if !reported && ev != nil && ev.TaskInfo().TaskID != "" {
				reported = true
				mu.Lock()
				stopped := aborted
				seen = !stopped
				mu.Unlock()
				if stopped {
					// The report came after the caller stopped the send: the message did arrive. The
					// stop ends the stream; the caller settles by looking the message up.
					first <- context.Cause(ctx)
					return
				}
				first <- nil
			}
		}
		if !reported {
			first <- errors.New("hitl: kagent's stream ended without a task")
		}
	}()
	select {
	case err := <-first:
		return err
	case <-ctx.Done():
	}
	mu.Lock()
	if seen {
		mu.Unlock()
		return <-first
	}
	aborted = true
	mu.Unlock()
	cancel()
	select {
	case err := <-first:
		return err
	case <-time.After(abortWait):
		return ctx.Err() // the send may have left; the caller looks the message up
	}
}
