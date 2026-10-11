package hitl

import (
	"context"
	"errors"
	"time"

	"github.com/oldsj/mainloop/server/internal/kagent"
)

// turnCap bounds how long the stream of a sent response is read after SendHITL has returned. The
// response resumes the task, so the stream carries the rest of the agent's turn; cutting it off
// early could end that turn.
const turnCap = 30 * time.Minute

// responseText is the text part of a HITL response message. kagent reads the answer from the
// extension metadata; the text only has to be non-empty.
const responseText = "HITL response"

// FromClient adapts the shared kagent client.
func FromClient(c *kagent.Client) Kagent { return clientAdapter{c} }

type clientAdapter struct{ *kagent.Client }

// SendHITL sends the response as one user message to the waiting task, with the answer in the
// HITL extension metadata and the extension named on the message (the client also puts it in the
// A2A-Extensions header of every call). It returns when kagent first reports the task, when the
// send fails, or when ctx ends; the stream is then read on in the background so the agent's turn
// is not cut short. Errors keep their kagent types, so kagent.NotSent still tells a send that
// never left from one that may have.
func (a clientAdapter) SendHITL(ctx context.Context, agent kagent.AgentRef, s Send) error {
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnCap)
	first := make(chan error, 1) // receives exactly one value
	go func() {
		defer cancel()
		seen := false
		for ev, err := range a.SendMessage(run, agent, kagent.SendParams{
			Text: responseText, MessageID: s.MessageID, ContextID: s.ContextID, TaskID: s.TaskID,
			Metadata:   map[string]any{kagent.HITLExtension: s.Native},
			Extensions: []string{kagent.HITLExtension},
		}) {
			if err != nil {
				if !seen {
					first <- err
				}
				return
			}
			if !seen && ev != nil && ev.TaskInfo().TaskID != "" {
				seen = true
				first <- nil
			}
		}
		if !seen {
			first <- errors.New("hitl: kagent's stream ended without a task")
		}
	}()
	select {
	case err := <-first:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
