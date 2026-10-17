package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// carryMessageChars cuts each carried message, so the preamble stays small however long the earlier
// turns were.
const carryMessageChars = 600

// mainContext opens the first message to every main Session: what the agent is, then the guidance
// every Mainloop agent gets. Its first line carries the marker, and no other line starts with one,
// so the carried messages after it cannot be mistaken for it.
const mainContext = "[mainloop:context] You are the Mainloop main agent, the owner's home chat. " +
	"You coordinate and delegate code work with `delegate`; tasks report back to you.\n\n" + agentcontext.Guidance

// frozenText returns the text a delivery sends and freezes it in the transaction that moves the row
// to sending, so one A2A messageId always carries one payload.
//
// A native Session is born without the conversation the owner has had. The first message to a main
// Session (a first use, a provider switch, or a replacement for one kagent deleted) therefore carries
// a short preamble of the last completed messages. The binding's standing_hash says whether this has
// been done: NULL means the Session has not been given it, and the switch and the replacement of a
// deleted Session both reset it. A Session that has its standing context but was given an older
// guidance version (or none, as every main that predates the guidance) gets the context block alone
// in front of its next message; the binding's guidance_version says which. The owner's mirrored
// message stays what they wrote.
//
// The preamble opens with mainContext, then the earlier messages when there are any. It does not
// render the main's standing context yet (see delegation.md). Children and workers are not main
// bindings and send their message as written. It returns a nil text when the message goes as
// written, and a non-nil record only when the caller must record what was given on the binding
// (after the delivery row, see decideSend).
func (t *turn) frozenText(ctx context.Context, q *dbq.Queries) (text *string, given *givenContext, err error) {
	if t.d.SentText != nil {
		return t.d.SentText, nil, nil
	}
	if dbstate.BindingRole(t.d.BindingRole) != dbstate.RoleMain {
		return nil, nil, nil
	}
	b, err := q.GetAgentBinding(ctx, t.d.SessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("load binding: %w", err)
	}
	if b.StandingHash != nil {
		if b.GuidanceVersion != nil && *b.GuidanceVersion == agentcontext.Version {
			return nil, nil, nil
		}
		rendered := t.withContext("")
		return &rendered, &givenContext{}, nil
	}
	earlier, err := t.carriedConversation(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	rendered := t.withContext(earlier)
	return &rendered, &givenContext{standingHash: ptrStr(standingHash(t.c.ID, rendered))}, nil
}

// givenContext is what a delivery that froze context text records on the binding. A nil
// standingHash means the Session already had its standing context and was given only the guidance.
type givenContext struct {
	standingHash *string
}

// standingHash is what the binding records for a delivery that froze this text. It includes the
// delivery, so giving the context back (ClearBindingStandingHash) only undoes that delivery's own
// freeze, even when another delivery froze byte-identical text.
func standingHash(deliveryID, text string) string {
	sum := sha256.Sum256([]byte(deliveryID + "\n" + text))
	return hex.EncodeToString(sum[:])
}

// withContext renders the message with the preamble in front of it: mainContext, then the earlier
// conversation when there is any to carry.
func (t *turn) withContext(earlier string) string {
	return mainContext + earlier + newMessageMarker + "\n" + t.d.Content
}

// newMessageMarker ends the preamble; the owner's message follows it.
const newMessageMarker = "[mainloop:new-message]"

// carriedConversation renders the last completed messages as marker-led lines, or "" when there is
// nothing to carry.
func (t *turn) carriedConversation(ctx context.Context, q *dbq.Queries) (string, error) {
	n := t.s.opts().CarryOverMessages
	if n <= 0 {
		return "", nil
	}
	// Each completed delivery is one user message and at most one mirrored reply, so the last n
	// messages are among the last n deliveries. Failed, cancelled, uncertain and still-open
	// deliveries are left out with their replies, partial ones and stop notes included.
	done, err := q.ListCompletedDeliveries(ctx, dbq.ListCompletedDeliveriesParams{SessionID: t.d.SessionID, Limit: int32(n)}) //nolint:gosec // capped at 100 by validation
	if err != nil {
		return "", fmt.Errorf("list completed deliveries: %w", err)
	}
	ids := make([]string, 0, 2*len(done))
	for _, d := range done {
		ids = append(ids, d.ID)
		if d.TaskID != nil {
			ids = append(ids, assistantID(t.d.SessionID, a2a.TaskID(*d.TaskID)))
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	msgs, err := q.ListSessionMessagesByID(ctx, dbq.ListSessionMessagesByIDParams{SessionID: t.d.SessionID, Ids: ids})
	if err != nil {
		return "", fmt.Errorf("list carried messages: %w", err)
	}
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	if len(msgs) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("[mainloop:carry-over] Earlier conversation, carried over: this session is new and has not seen it. " +
		"It is background only; do not act on it again. Your new message follows the last line of it.\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, carried(m.Content))
	}
	return b.String(), nil
}

// lineBreaks indents every kind of line break, so a carried message cannot start a line of its own.
var lineBreaks = strings.NewReplacer(
	"\r\n", "\r\n  ", "\n", "\n  ", "\r", "\r  ", "\v", "\v  ", "\f", "\f  ",
	"\u0085", "\u0085  ", "\u2028", "\u2028  ", "\u2029", "\u2029  ",
)

// carried cuts a message and indents its later lines, so nothing inside it can pass for a marker
// line of the preamble.
func carried(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > carryMessageChars {
		s = string(r[:carryMessageChars]) + "…"
	}
	return lineBreaks.Replace(s)
}

// returnStandingContext gives back, in the transaction that ends a delivery whose message kagent
// never saw, the standing context and the guidance version that delivery froze, so the next message
// carries them again.
func (t *turn) returnStandingContext(ctx context.Context, tx pgx.Tx, final dbstate.DeliveryState, task *a2a.Task) error {
	if (final != dbstate.DeliveryFailed && final != dbstate.DeliveryCancelled) || task != nil || t.taskID != "" {
		return nil
	}
	q := dbq.New(tx)
	if err := q.ClearBindingStandingHash(ctx, t.c.ID); err != nil {
		return fmt.Errorf("return standing context: %w", err)
	}
	if err := q.ClearBindingGuidanceVersion(ctx, &t.c.ID); err != nil {
		return fmt.Errorf("return guidance version: %w", err)
	}
	return nil
}
