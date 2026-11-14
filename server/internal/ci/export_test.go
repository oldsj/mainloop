package ci

import (
	"context"

	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// HandleWatch runs the watch loop's handler on one claim.
func (s *Service) HandleWatch(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	return s.handleWatch(ctx, c)
}
