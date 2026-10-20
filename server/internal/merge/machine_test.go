package merge

import (
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

func TestTransitionsAreExactlyTheEdges(t *testing.T) {
	t.Parallel()
	all := []dbstate.MergeState{
		dbstate.MergePrepared, dbstate.MergeEvaluating, dbstate.MergeMerging, dbstate.MergeUncertain,
		dbstate.MergeBlocked, dbstate.MergeExpired, dbstate.MergeSuperseded, dbstate.MergeMerged,
	}
	allowed := map[[2]dbstate.MergeState]bool{
		{dbstate.MergePrepared, dbstate.MergeEvaluating}:   true,
		{dbstate.MergePrepared, dbstate.MergeSuperseded}:   true,
		{dbstate.MergePrepared, dbstate.MergeBlocked}:      true,
		{dbstate.MergeEvaluating, dbstate.MergeMerging}:    true,
		{dbstate.MergeEvaluating, dbstate.MergeBlocked}:    true,
		{dbstate.MergeEvaluating, dbstate.MergeExpired}:    true,
		{dbstate.MergeEvaluating, dbstate.MergeSuperseded}: true,
		{dbstate.MergeMerging, dbstate.MergeMerged}:        true,
		{dbstate.MergeMerging, dbstate.MergeUncertain}:     true,
		{dbstate.MergeMerging, dbstate.MergeBlocked}:       true,
		{dbstate.MergeMerging, dbstate.MergeEvaluating}:    true,
		{dbstate.MergeUncertain, dbstate.MergeUncertain}:   true,
		{dbstate.MergeUncertain, dbstate.MergeMerged}:      true,
		{dbstate.MergeUncertain, dbstate.MergeBlocked}:     true,
	}
	for _, from := range all {
		for _, to := range all {
			err := Transition(from, to)
			if allowed[[2]dbstate.MergeState{from, to}] {
				if err != nil {
					t.Errorf("%s -> %s refused: %v", from, to, err)
				}
			} else if !errors.Is(err, ErrTransition) {
				t.Errorf("%s -> %s = %v, want ErrTransition", from, to, err)
			}
		}
		wantFinal := from == dbstate.MergeBlocked || from == dbstate.MergeExpired || from == dbstate.MergeSuperseded || from == dbstate.MergeMerged
		if Final(from) != wantFinal {
			t.Errorf("Final(%s) = %v", from, Final(from))
		}
	}
}
