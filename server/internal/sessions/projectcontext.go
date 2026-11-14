package sessions

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// projectGiven is the project context a delivery to a task session gives it: the instructions
// version and memory sequence it brings the session up to.
type projectGiven struct {
	instructionsVersion int32
	memorySeq           int64
}

// projectUpdate is what a task session's next message carries when its project's instructions or
// memory changed since the session was last given them (docs/specs/project-context.md#running-sessions):
// the changes, as a block for the front of the message, and what to record on the delivery. A
// session has what the newest delivery that gave it a change gave, unless kagent never saw that
// message, else what its attempt's brief pinned; a session whose brief is not pinned yet, or with
// no project, gets nothing. The block is "" when the versions moved without a change the session
// reads (an entry added and removed again); the delivery still records them, so the next message
// does not look again.
func (t *turn) projectUpdate(ctx context.Context, q *dbq.Queries) (string, *projectGiven, error) {
	if dbstate.BindingRole(t.d.BindingRole) == dbstate.RoleMain {
		return "", nil, nil
	}
	row, err := q.ProjectContextGiven(ctx, dbq.ProjectContextGivenParams{SessionID: t.d.SessionID, DeliveryID: t.c.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("project context given: %w", err)
	}
	if row.InstructionsVersion == nil {
		return "", nil, nil
	}
	var seq int64
	if row.MemorySeq != nil {
		seq = *row.MemorySeq
	}
	up, err := projectcontext.UpdateSince(ctx, q, row.ProjectID, int(*row.InstructionsVersion), seq)
	if err != nil {
		return "", nil, err
	}
	if up.InstructionsVersion == int(*row.InstructionsVersion) && up.MemorySeq == seq {
		return "", nil, nil
	}
	return up.Block, &projectGiven{
		instructionsVersion: int32(up.InstructionsVersion), //nolint:gosec // versions stay far below 2^31
		memorySeq:           up.MemorySeq,
	}, nil
}
