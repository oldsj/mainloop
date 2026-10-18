package devloop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
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
