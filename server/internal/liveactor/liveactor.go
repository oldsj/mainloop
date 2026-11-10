// Package liveactor names the running actor of a kagent Session for the Substrate router. It is a
// leaf shared by the preview proxy, the live relay and the session service, which cannot import
// one another.
package liveactor

import (
	"errors"
	"regexp"

	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// ErrNotFound is a session the owner cannot open live.
var ErrNotFound = errors.New("session is not live")

// ErrWaking is a session whose actor is not running yet.
var ErrWaking = errors.New("session is waking")

// Target is the actor serving a live session: its kagent Session and the router's
// "<atespace>/<actor>" name for the current runtime generation.
type Target struct {
	KagentSessionID string
	Actor           string
}

var actorSafe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}/[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// Valid reports whether actor is safe to put in the router's CONNECT request head.
func Valid(actor string) bool { return actorSafe.MatchString(actor) }

// Of is the router's "<atespace>/<actor>" name of the Session's running actor, from its runtime
// association. kagent reports the association only while the actor runs, and the actor name
// carries the runtime generation, so it cannot be derived from the Session id.
func Of(ks *kagentapi.Session) (string, bool) {
	ra := ks.GetRuntimeAssociation()
	if ra.GetAtespace() == "" || ra.GetActorName() == "" {
		return "", false
	}
	actor := ra.GetAtespace() + "/" + ra.GetActorName()
	return actor, Valid(actor)
}
