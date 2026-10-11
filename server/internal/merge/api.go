package merge

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

var errUnavailable = apierr.Unavailable("merge details are not available")

// MergeAPI serves the owner's read of a merge proposal's details.
type MergeAPI struct {
	Svc     *Service
	OwnerID string
}

// GetHITLMergeDetails implements api.StrictServerInterface.
func (a *MergeAPI) GetHITLMergeDetails(ctx context.Context, req api.GetHITLMergeDetailsRequestObject) (api.GetHITLMergeDetailsResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	in := DetailsInput{OwnerID: a.OwnerID, HITLRequestID: req.RequestId, ProposalID: req.ProposalId, Section: req.Params.Section}
	if req.Params.Cursor != nil {
		in.Cursor = *req.Params.Cursor
	}
	if req.Params.Limit != nil {
		in.Limit = *req.Params.Limit
	}
	out, err := a.Svc.Details(ctx, in)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Merge proposal not found")
	case errors.Is(err, ErrTooLarge):
		return nil, apierr.Conflict("A single entry of the details is too large to show")
	case err != nil:
		return nil, err
	}
	return api.GetHITLMergeDetails200JSONResponse(out), nil
}
