package devloop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestDevloopAPIActsAsTheOwnerHolder(t *testing.T) {
	d := testdb.New(t)
	a := devloop.NewAPI(&devloop.LeaseService{Pool: d.Pool})
	ctx := context.Background()

	got, err := a.AcquireDevloopLease(ctx, api.AcquireDevloopLeaseRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	acquired, ok := got.(api.AcquireDevloopLease200JSONResponse)
	if !ok || acquired.Status != api.DevloopLeaseStatusAcquired {
		t.Fatalf("AcquireDevloopLease = %#v, want status acquired", got)
	}
	if acquired.HolderSession == nil || *acquired.HolderSession != devloop.OwnerHolder {
		t.Fatalf("AcquireDevloopLease holder = %v, want %q", acquired.HolderSession, devloop.OwnerHolder)
	}

	read, err := a.GetDevloopLease(ctx, api.GetDevloopLeaseRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := read.(api.GetDevloopLease200JSONResponse); !ok || v.Status != api.DevloopLeaseStatusAcquired {
		t.Fatalf("GetDevloopLease = %#v, want status acquired", read)
	}

	released, err := a.ReleaseDevloopLease(ctx, api.ReleaseDevloopLeaseRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := released.(api.ReleaseDevloopLease200JSONResponse); !ok || v.Status != api.DevloopLeaseStatusFree {
		t.Fatalf("ReleaseDevloopLease = %#v, want status free", released)
	}
}

func TestDevloopAPIWithNoLeaseServiceIsUnavailable(t *testing.T) {
	a := devloop.NewAPI(nil)
	ctx := context.Background()
	if _, err := a.GetDevloopLease(ctx, api.GetDevloopLeaseRequestObject{}); !isUnavailable(err) {
		t.Fatalf("GetDevloopLease with no Lease = %v, want 503 unavailable", err)
	}
	if _, err := a.AcquireDevloopLease(ctx, api.AcquireDevloopLeaseRequestObject{}); !isUnavailable(err) {
		t.Fatalf("AcquireDevloopLease with no Lease = %v, want 503 unavailable", err)
	}
	if _, err := a.ReleaseDevloopLease(ctx, api.ReleaseDevloopLeaseRequestObject{}); !isUnavailable(err) {
		t.Fatalf("ReleaseDevloopLease with no Lease = %v, want 503 unavailable", err)
	}
}

func isUnavailable(err error) bool {
	var ae *apierr.Error
	return errors.As(err, &ae) && ae.Status == 503
}

func TestListDevloopOperationsShowsEachSyncsVerdict(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	insert := func(id, session, sha string, state dbstate.DevloopOperationState, result, detail string, age time.Duration) {
		t.Helper()
		var res, det any
		if result != "" {
			res = result
		}
		if detail != "" {
			det = detail
		}
		if _, err := d.Pool.Exec(ctx, `INSERT INTO devloop_operations (id, idempotency_key, kind, session_id, project_id, params, state, result, detail, created_at)
			VALUES ($1, $1, 'sync', $2, 'p', $3::jsonb, $4, $5::jsonb, $6, now() - $7::float8 * interval '1 second')`,
			id, session, `{"repo":"o/n","commit_sha":"`+sha+`"}`, string(state), res, det, age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	insert("ok", "s1", "aaaa", dbstate.DevloopSucceeded,
		`{"migrate_ok":true,"health_ok":true,"smoke":{"passed":true,"summary":"summary: 8 ok, 0 fail, 2 skip"}}`, "", time.Minute)
	insert("bad", "s1", "bbbb", dbstate.DevloopFailed, `{"migrate_ok":true,"health_ok":false}`, "the backend did not report healthy", 2*time.Minute)
	insert("other", "s2", "cccc", dbstate.DevloopFailed, "", "no running pod", 3*time.Minute)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO devloop_operations (id, idempotency_key, kind, session_id, project_id, params) VALUES ('st', 'st', 'status', 's1', 'p', '{}')`); err != nil {
		t.Fatal(err)
	}
	a := devloop.NewAPI(&devloop.LeaseService{Pool: d.Pool})

	session := "s1"
	got, err := a.ListDevloopOperations(ctx, api.ListDevloopOperationsRequestObject{Params: api.ListDevloopOperationsParams{SessionId: &session}})
	if err != nil {
		t.Fatal(err)
	}
	rows := got.(api.ListDevloopOperations200JSONResponse)
	if len(rows) != 2 || rows[0].Id != "ok" || rows[1].Id != "bad" {
		t.Fatalf("session s1 syncs = %+v, want ok then bad (newest first, no status rows, no other session)", rows)
	}
	first, second := rows[0], rows[1]
	if first.CommitSha != "aaaa" || first.State != api.DevloopOperationStateSucceeded ||
		first.HealthOk == nil || !*first.HealthOk || first.MigrateOk == nil || !*first.MigrateOk ||
		first.Smoke == nil || !first.Smoke.Passed || first.Smoke.Summary != "summary: 8 ok, 0 fail, 2 skip" {
		t.Errorf("ok row = %+v", first)
	}
	if second.HealthOk == nil || *second.HealthOk || second.Smoke != nil || second.Detail == nil || *second.Detail != "the backend did not report healthy" {
		t.Errorf("bad row = %+v, want health_ok false, no smoke, its detail", second)
	}

	all, err := a.ListDevloopOperations(ctx, api.ListDevloopOperationsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(all.(api.ListDevloopOperations200JSONResponse)); n != 3 {
		t.Errorf("no session filter returned %d syncs, want 3", n)
	}
}

func TestListDevloopOperationsWithNoLeaseServiceIsUnavailable(t *testing.T) {
	if _, err := devloop.NewAPI(nil).ListDevloopOperations(context.Background(), api.ListDevloopOperationsRequestObject{}); !isUnavailable(err) {
		t.Fatalf("ListDevloopOperations with no Lease = %v, want 503 unavailable", err)
	}
}
