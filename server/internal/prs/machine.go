// Package prs opens pull requests for agents. A call writes an intent row first; a reconcile loop
// then lists the head's pull requests on GitHub and posts at most once. A POST whose outcome is not
// known is never repeated: the loop looks the pull request up by its head until it appears or the
// window for it closes.
package prs

import (
	"errors"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// ErrTransition is returned for a move the state machine does not allow.
var ErrTransition = errors.New("prs: transition not allowed")

// edges are the moves of a pull-request creation. dispatching -> pending is the one way back: a
// POST that GitHub rate-limited provably did not run.
var edges = map[dbstate.PRCreationState][]dbstate.PRCreationState{
	dbstate.PRPending:     {dbstate.PRDispatching, dbstate.PRCreated, dbstate.PRRefused, dbstate.PRUncertain},
	dbstate.PRDispatching: {dbstate.PRPending, dbstate.PRCreated, dbstate.PRRefused, dbstate.PRUncertain},
	dbstate.PRUncertain:   {dbstate.PRUncertain, dbstate.PRCreated, dbstate.PRRefused},
}

// Final reports whether a creation can no longer change.
func Final(s dbstate.PRCreationState) bool {
	return s == dbstate.PRCreated || s == dbstate.PRRefused
}

// Transition checks a move.
func Transition(from, to dbstate.PRCreationState) error {
	if slices.Contains(edges[from], to) {
		return nil
	}
	if Final(from) {
		return fmt.Errorf("%w: creation is %s", ErrTransition, from)
	}
	return fmt.Errorf("%w: creation cannot move from %s to %s", ErrTransition, from, to)
}
