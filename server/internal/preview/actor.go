package preview

import (
	"context"
	"errors"

	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// SessionObserver reads a kagent Session; *kagent.Client satisfies it.
type SessionObserver interface {
	ObserveSession(ctx context.Context, id string) (*kagentapi.Session, error)
}

// errNoActor is a kagent Session with no running actor yet (waking, or between generations).
var errNoActor = errors.New("kagent session has no running actor")

// RuntimeActor is the router's "<atespace>/<actor>" name of the Session's running actor, from its
// runtime association. kagent reports the association only while the actor runs, and the actor
// name carries the runtime generation, so it cannot be derived from the Session id.
func RuntimeActor(ks *kagentapi.Session) (string, bool) {
	ra := ks.GetRuntimeAssociation()
	if ra.GetAtespace() == "" || ra.GetActorName() == "" || !actorSafe.MatchString(ra.GetAtespace()+"/"+ra.GetActorName()) {
		return "", false
	}
	return ra.GetAtespace() + "/" + ra.GetActorName(), true
}

// actorOf reads the running actor of a kagent Session.
func (p *Proxy) actorOf(ctx context.Context, kagentSessionID string) (string, error) {
	ks, err := p.Sessions.ObserveSession(ctx, kagentSessionID)
	if err != nil {
		return "", err
	}
	actor, ok := RuntimeActor(ks)
	if !ok {
		return "", errNoActor
	}
	return actor, nil
}
