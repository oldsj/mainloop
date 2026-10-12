package preview

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

const (
	// keepAwakeEvery is how often the sweep touches workspaces that hold a lease.
	keepAwakeEvery = 20 * time.Second
	// leaseKeep is how long an expired lease row is kept before it is pruned.
	leaseKeep = time.Minute
	dbTimeout = 5 * time.Second
)

// Register adds the keep-awake sweep to the runner.
func (p *Proxy) Register(r *reconcile.Runner) {
	r.AddSweep(reconcile.Sweep{Name: "preview-keep-awake", Every: keepAwakeEvery, Run: p.KeepAwake})
}

// KeepAwake counts every workspace with an unexpired lease as active, which defers the idle
// suspend, and prunes leases whose holder stopped renewing them. The lease rows are the state: a
// crashed holder's lease lapses by itself, and any process can run this.
func (p *Proxy) KeepAwake(ctx context.Context) error {
	q := dbq.New(p.Pool)
	ids, err := q.ListLeasedWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := p.Waker.Touch(ctx, id); err != nil {
			p.Log.WarnContext(ctx, "preview activity touch failed", "workspace", id, "error", err)
		}
	}
	_, err = q.PruneExpiredPreviewLeases(ctx, leaseKeep.Seconds())
	return err
}

// lease is one open stream's keep-awake lease.
type lease struct {
	p           *Proxy
	id          string
	workspaceID string

	mu     sync.Mutex
	held   bool
	closed bool
	wait   *time.Timer
	stop   chan struct{}
	done   chan struct{}
}

// holdLease starts a lease for a stream on the workspace. A WebSocket takes it at once; an HTTP
// request only if it is still running after HTTPLeaseWait, so short requests cost no writes
// beyond the touch. Call release when the stream ends.
func (p *Proxy) holdLease(workspaceID string, immediate bool) *lease {
	l := &lease{p: p, id: uuid.NewString(), workspaceID: workspaceID, stop: make(chan struct{}), done: make(chan struct{})}
	if immediate {
		l.acquire()
	} else {
		l.wait = time.AfterFunc(p.HTTPLeaseWait, l.acquire)
	}
	return l
}

func (l *lease) acquire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.held {
		return
	}
	if err := l.write(); err != nil {
		l.p.Log.Warn("preview lease not recorded", "workspace", l.workspaceID, "error", err)
		return
	}
	l.held = true
	go l.renew()
}

func (l *lease) write() error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	return dbq.New(l.p.Pool).UpsertPreviewLease(ctx, dbq.UpsertPreviewLeaseParams{
		ID: l.id, WorkspaceID: l.workspaceID, Holder: l.p.holder, TtlSeconds: l.p.LeaseTTL.Seconds(),
	})
}

// renew extends the lease until release. A failed renewal is logged and retried at the next
// tick; the lease only lapses if the database stays unreachable for the whole TTL.
func (l *lease) renew() {
	defer close(l.done)
	t := time.NewTicker(l.p.RenewEvery)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if err := l.write(); err != nil {
				l.p.Log.Warn("preview lease renewal failed", "workspace", l.workspaceID, "error", err)
			}
		}
	}
}

// release ends the lease and deletes its row.
func (l *lease) release() {
	l.mu.Lock()
	l.closed = true
	held := l.held
	if l.wait != nil {
		l.wait.Stop()
	}
	l.mu.Unlock()
	if !held {
		return
	}
	close(l.stop)
	<-l.done
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := dbq.New(l.p.Pool).DeletePreviewLease(ctx, l.id); err != nil {
		// The row expires on its own.
		l.p.Log.Warn("preview lease not removed", "workspace", l.workspaceID, "error", err)
	}
}
