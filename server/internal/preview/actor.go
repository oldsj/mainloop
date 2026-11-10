package preview

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/liveactor"

	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// SessionObserver reads a kagent Session; *kagent.Client satisfies it.
type SessionObserver interface {
	ObserveSession(ctx context.Context, id string) (*kagentapi.Session, error)
}

// errNoActor is a kagent Session with no running actor yet (waking, or between generations).
var errNoActor = errors.New("kagent session has no running actor")

// actorOf reads the running actor of a kagent Session.
func (p *Proxy) actorOf(ctx context.Context, kagentSessionID string) (string, error) {
	ks, err := p.Sessions.ObserveSession(ctx, kagentSessionID)
	if err != nil {
		return "", err
	}
	actor, ok := liveactor.Of(ks)
	if !ok {
		return "", errNoActor
	}
	return actor, nil
}
