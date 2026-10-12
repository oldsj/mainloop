package smoke

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReadFailureKeepsAnImmediateErrorAfterCancel(t *testing.T) {
	readErr := errors.New("GitHub read failed")
	run := t.Context()

	// The caller cancels the step context before classifying, as poll does.
	step, cancel := context.WithDeadline(run, time.Now().Add(time.Hour))
	cancel()
	if got := readFailure(run, step, readErr); !errors.Is(got, readErr) {
		t.Fatalf("readFailure() = %v, want the read error", got)
	}

	expired, cancelExpired := context.WithDeadline(run, time.Now().Add(-time.Second))
	defer cancelExpired()
	if got := readFailure(run, expired, readErr); got == nil || got.Error() != "step deadline" {
		t.Fatalf("readFailure() = %v, want step deadline", got)
	}

	runExpired, cancelRun := context.WithDeadline(run, time.Now().Add(-time.Second))
	defer cancelRun()
	if got := readFailure(runExpired, expired, readErr); got == nil || got.Error() != "overall deadline" {
		t.Fatalf("readFailure() = %v, want overall deadline", got)
	}
}
