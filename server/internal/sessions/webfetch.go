package sessions

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// SetWebFetch turns the session's own web_fetch flag on or off (docs/specs/web-fetch.md). The
// agent also holds the tool while its task carries the flag, which this does not change. The
// session must be the owner's.
func (s *Service) SetWebFetch(ctx context.Context, sessionID string, enabled bool) (api.SessionWebFetch, error) {
	row, err := dbq.New(s.Pool).SetBindingWebFetch(ctx, dbq.SetBindingWebFetchParams{
		SessionID: sessionID, UserID: s.OwnerID, WebFetch: enabled,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.SessionWebFetch{}, ErrNotFound
	}
	if err != nil {
		return api.SessionWebFetch{}, fmt.Errorf("set web fetch: %w", err)
	}
	return api.SessionWebFetch{SessionId: row.SessionID, Enabled: row.WebFetch, FromTask: row.FromTask}, nil
}
