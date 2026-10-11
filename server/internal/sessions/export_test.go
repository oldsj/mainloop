package sessions

import (
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// ErrorDelay is the wait before a task is read again after a failed read, for a delivery that has
// been claimed attempts times.
func ErrorDelay(o Options, uncertain bool, attempts int) time.Duration {
	t := &turn{s: &Service{Opts: o}, c: reconcile.Claim{Attempts: attempts}, cur: dbstate.DeliveryDelivered}
	if uncertain {
		t.cur = dbstate.DeliveryUncertain
	}
	return t.errorDelay()
}

// ParkedDelay is the wait before a task parked for an answer is read again.
func ParkedDelay(o Options, attempts int) time.Duration {
	t := &turn{s: &Service{Opts: o}, c: reconcile.Claim{Attempts: attempts}}
	return t.parkedDelay()
}
