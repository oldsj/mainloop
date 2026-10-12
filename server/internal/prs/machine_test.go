package prs_test

import (
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/prs"
)

func TestTransitionsAreExactlyTheEdges(t *testing.T) {
	all := []dbstate.PRCreationState{dbstate.PRPending, dbstate.PRDispatching, dbstate.PRUncertain, dbstate.PRCreated, dbstate.PRRefused}
	allowed := map[[2]dbstate.PRCreationState]bool{
		{dbstate.PRPending, dbstate.PRDispatching}:   true,
		{dbstate.PRPending, dbstate.PRCreated}:       true,
		{dbstate.PRPending, dbstate.PRRefused}:       true,
		{dbstate.PRPending, dbstate.PRUncertain}:     true,
		{dbstate.PRDispatching, dbstate.PRPending}:   true,
		{dbstate.PRDispatching, dbstate.PRCreated}:   true,
		{dbstate.PRDispatching, dbstate.PRRefused}:   true,
		{dbstate.PRDispatching, dbstate.PRUncertain}: true,
		{dbstate.PRUncertain, dbstate.PRUncertain}:   true,
		{dbstate.PRUncertain, dbstate.PRCreated}:     true,
		{dbstate.PRUncertain, dbstate.PRRefused}:     true,
	}
	for _, from := range all {
		for _, to := range all {
			err := prs.Transition(from, to)
			if allowed[[2]dbstate.PRCreationState{from, to}] {
				if err != nil {
					t.Errorf("%s -> %s refused: %v", from, to, err)
				}
			} else if !errors.Is(err, prs.ErrTransition) {
				t.Errorf("%s -> %s = %v, want ErrTransition", from, to, err)
			}
		}
		if got, want := prs.Final(from), from == dbstate.PRCreated || from == dbstate.PRRefused; got != want {
			t.Errorf("Final(%s) = %v", from, got)
		}
	}
}
