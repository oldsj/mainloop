package pushgate

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// All listeners in a process share this budget. A pod owns its spool volume; branch admission
// is separately stored in PostgreSQL so listeners in other pods cannot admit the same branch.
var receiveScratch struct {
	sync.Mutex
	used int64
}

func reserveScratch(l Limits) (func(), error) {
	r, err := reserveScratchReservation(l)
	if err != nil {
		return nil, err
	}
	return r.release, nil
}

type scratchReservation struct{ size int64 }

func reserveScratchReservation(l Limits) (*scratchReservation, error) {
	size := min(l.BodyBytes, l.DiskBytes)
	receiveScratch.Lock()
	defer receiveScratch.Unlock()
	if size > l.DiskBytes-receiveScratch.used {
		return nil, denial("admission_busy")
	}
	receiveScratch.used += size
	return &scratchReservation{size: size}, nil
}

// Validation reserves its complete per-quarantine disk allowance before fetching a seed or
// starting Git. This conservatively serializes validation at the pod's disk budget, including
// subprocess temporary files, while independent branches can still spool bodies concurrently.
func (r *scratchReservation) validation(l Limits) error {
	receiveScratch.Lock()
	defer receiveScratch.Unlock()
	if l.DiskBytes-r.size > l.DiskBytes-receiveScratch.used {
		return denial("admission_busy")
	}
	receiveScratch.used += l.DiskBytes - r.size
	r.size = l.DiskBytes
	return nil
}

func (r *scratchReservation) release() {
	receiveScratch.Lock()
	defer receiveScratch.Unlock()
	receiveScratch.used -= r.size
	r.size = 0
}

func (s *Service) admit(ctx context.Context, c capability, a authority, owner string, l Limits) (string, error) {
	id := publicationID(c, owner)
	// The lease outlives body/validation (at most 3*Timeout), detached dispatch (Timeout),
	// and outcome persistence (10 seconds). Contexts and HTTP read deadlines stop stale I/O;
	// publication reservation and dispatch also lock and check this lease before proceeding.
	until := time.Now().Add(4*l.Timeout + 30*time.Second)
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.LockPushWriter(ctx, dbq.LockPushWriterParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch}); err != nil {
			return err
		}
		cur, err := load(ctx, q, c)
		if err != nil {
			return err
		}
		if !sameAuthority(a, cur) {
			return denial("authority_changed")
		}
		busy, err := q.PushAdmissionHeld(ctx, dbq.PushAdmissionHeldParams{Repository: a.W.Repository, Branch: a.W.Branch})
		if err != nil {
			return err
		}
		if busy {
			return denial("admission_busy")
		}
		held, err := q.PushBranchHeld(ctx, dbq.PushBranchHeldParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch})
		if err != nil {
			return err
		}
		if held {
			return denial("publication_unresolved")
		}
		n, err := q.AcquirePushAdmission(ctx, dbq.AcquirePushAdmissionParams{Repository: a.W.Repository, Branch: a.W.Branch, ID: id, LeaseUntil: until})
		if err != nil {
			return err
		}
		if n != 1 {
			return denial("admission_busy")
		}
		return nil
	})
	return id, err
}

func requireAdmission(ctx context.Context, q *dbq.Queries, id string) error {
	_, err := q.LockPushAdmission(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return denial("admission_busy")
	}
	return err
}

func (s *Service) releaseAdmission(ctx context.Context, id string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// An unavailable database leaves only a bounded lease, never a permanent branch fence.
	_ = dbq.New(s.Pool).ReleasePushAdmission(cleanup, id)
}
