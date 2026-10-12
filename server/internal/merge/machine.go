// Package merge prepares, approves and carries out pull-request merges. A preparation freezes the
// facts the owner decides on. A merge is a durable intent row that a reconcile loop evaluates until
// the evidence holds or a deadline passes; the one PUT to GitHub is sent only from a claimed row
// whose merging state is already committed, and a PUT whose outcome is not known is settled by
// reading the pull request, never by sending it again.
package merge

import (
	"errors"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// ErrTransition is returned for a move the state machine does not allow.
var ErrTransition = errors.New("merge: transition not allowed")

// edges are the moves of a merge candidate. merging -> evaluating is the one way back: a PUT that
// GitHub rate-limited provably did not run.
var edges = map[dbstate.MergeState][]dbstate.MergeState{
	dbstate.MergePrepared:   {dbstate.MergeEvaluating, dbstate.MergeSuperseded, dbstate.MergeBlocked},
	dbstate.MergeEvaluating: {dbstate.MergeMerging, dbstate.MergeBlocked, dbstate.MergeExpired, dbstate.MergeSuperseded},
	dbstate.MergeMerging:    {dbstate.MergeMerged, dbstate.MergeUncertain, dbstate.MergeBlocked, dbstate.MergeEvaluating},
	dbstate.MergeUncertain:  {dbstate.MergeUncertain, dbstate.MergeMerged, dbstate.MergeBlocked},
}

// Final reports whether a candidate can no longer change.
func Final(s dbstate.MergeState) bool {
	switch s {
	case dbstate.MergeBlocked, dbstate.MergeExpired, dbstate.MergeSuperseded, dbstate.MergeMerged:
		return true
	}
	return false
}

// Transition checks a move.
func Transition(from, to dbstate.MergeState) error {
	if slices.Contains(edges[from], to) {
		return nil
	}
	if Final(from) {
		return fmt.Errorf("%w: candidate is %s", ErrTransition, from)
	}
	return fmt.Errorf("%w: candidate cannot move from %s to %s", ErrTransition, from, to)
}
