package transcripts

import (
	"context"
	"log/slog"
	"time"

	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// FlushTimeout bounds the copy taken before a kagent Session is deleted.
const FlushTimeout = 20 * time.Second

// FlushingKagent is the kagent client with every Session delete preceded by a [Service.Flush] of that
// Session, so its finished turns are copied while kagent can still serve them. The copy is bounded by
// [FlushTimeout] and never stops the delete: a copy that fails is logged, and the delete goes ahead.
type FlushingKagent struct {
	*kagent.Client
	flush   func(ctx context.Context, kagentSessionID string) error
	deleter sessionDeleter
	timeout time.Duration
	log     *slog.Logger
}

type sessionDeleter interface {
	DeleteSession(ctx context.Context, id string) (*kagentapi.Session, error)
	ForceDeleteSession(ctx context.Context, id string) (*kagentapi.Session, error)
}

// NewFlushingKagent wraps c so its deletes flush s first.
func NewFlushingKagent(c *kagent.Client, s *Service) *FlushingKagent {
	return &FlushingKagent{Client: c, flush: s.Flush, deleter: c, timeout: FlushTimeout, log: s.log()}
}

// DeleteSession copies the Session's finished turns, then deletes it.
func (k *FlushingKagent) DeleteSession(ctx context.Context, id string) (*kagentapi.Session, error) {
	k.beforeDelete(ctx, id)
	return k.deleter.DeleteSession(ctx, id)
}

// ForceDeleteSession copies the Session's finished turns, then force-deletes it.
func (k *FlushingKagent) ForceDeleteSession(ctx context.Context, id string) (*kagentapi.Session, error) {
	k.beforeDelete(ctx, id)
	return k.deleter.ForceDeleteSession(ctx, id)
}

func (k *FlushingKagent) beforeDelete(ctx context.Context, id string) {
	fctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	if err := k.flush(fctx, id); err != nil && ctx.Err() == nil {
		k.log.WarnContext(ctx, "transcripts: copy before delete failed; deleting anyway", "kagent_session_id", id, "error", err)
	}
}
