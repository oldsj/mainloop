package devloop

import (
	"context"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

// OwnerHolder is the lease holder key an operator's own acquire/release (this API, not an MCP
// tool) uses: distinct from any MCP session id, so the owner and a task agent never collide.
const OwnerHolder = "owner"

// DevloopAPI serves the owner's REST view of the lease: GET to read it without acquiring it,
// POST to acquire or release it as the owner. `make dev-live` acquires it this way (see
// docs/dev-environment.md); task agents use the dev_lease_acquire/dev_lease_release MCP tools.
type DevloopAPI struct {
	Lease *LeaseService
}

// NewAPI builds the DevloopAPI.
func NewAPI(lease *LeaseService) *DevloopAPI { return &DevloopAPI{Lease: lease} }

func leaseView(info LeaseInfo) api.DevloopLease {
	status := api.DevloopLeaseStatus(info.Status)
	if status == "" {
		status = api.DevloopLeaseStatusFree
	}
	out := api.DevloopLease{Status: status, IdleTimeoutSeconds: int(info.IdleTimeout.Seconds()), QueueLength: info.QueueLength}
	if info.HolderSession != "" {
		out.HolderSession = &info.HolderSession
	}
	out.AcquiredAt, out.LastUsedAt = info.AcquiredAt, info.LastUsedAt
	return out
}

// GetDevloopLease implements api.StrictServerInterface.
func (a *DevloopAPI) GetDevloopLease(ctx context.Context, _ api.GetDevloopLeaseRequestObject) (api.GetDevloopLeaseResponseObject, error) {
	if a.Lease == nil {
		return nil, apierr.Unavailable("devloop is not available")
	}
	info, err := a.Lease.Status(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetDevloopLease200JSONResponse(leaseView(info)), nil
}

// AcquireDevloopLease implements api.StrictServerInterface.
func (a *DevloopAPI) AcquireDevloopLease(ctx context.Context, _ api.AcquireDevloopLeaseRequestObject) (api.AcquireDevloopLeaseResponseObject, error) {
	if a.Lease == nil {
		return nil, apierr.Unavailable("devloop is not available")
	}
	info, err := a.Lease.Acquire(ctx, OwnerHolder)
	if err != nil {
		return nil, err
	}
	return api.AcquireDevloopLease200JSONResponse(leaseView(info)), nil
}

// ReleaseDevloopLease implements api.StrictServerInterface.
func (a *DevloopAPI) ReleaseDevloopLease(ctx context.Context, _ api.ReleaseDevloopLeaseRequestObject) (api.ReleaseDevloopLeaseResponseObject, error) {
	if a.Lease == nil {
		return nil, apierr.Unavailable("devloop is not available")
	}
	if err := a.Lease.Release(ctx, OwnerHolder); err != nil {
		return nil, err
	}
	info, err := a.Lease.Status(ctx)
	if err != nil {
		return nil, err
	}
	return api.ReleaseDevloopLease200JSONResponse(leaseView(info)), nil
}
