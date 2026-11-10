// Package liveactor names the running actor of a kagent Session for the Substrate router. It is a
// leaf shared by the preview proxy, the live relay and the session service, which cannot import
// one another.
package liveactor

import (
	"errors"
	"regexp"

	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
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

// Of is the router's "<atespace>/<actor>" name of the Session's Actor. It comes from the runtime
// association when kagent reports one, current or not. A sleeping Session may report none: kagent
// names a Session's Actor "session-<id>" for its whole life, so it is then built from that and
// defaultAtespace (the atespace Sessions run in). The caller's connection through the Substrate
// router wakes a sleeping Actor. ok is false for a Session that cannot have an Actor (not READY) or
// a name that is not safe for the CONNECT request head.
func Of(ks *kagentapi.Session, defaultAtespace string) (string, bool) {
	if ks.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_READY {
		return "", false
	}
	atespace, name := defaultAtespace, kagent.ActorName(ks.GetId())
	if ra := ks.GetRuntimeAssociation(); ra.GetAtespace() != "" && ra.GetActorName() != "" {
		atespace, name = ra.GetAtespace(), ra.GetActorName()
	}
	if atespace == "" || ks.GetId() == "" {
		return "", false
	}
	actor := atespace + "/" + name
	return actor, Valid(actor)
}
