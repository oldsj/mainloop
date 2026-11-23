package preview

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/auth"
)

// GrantAPI serves POST /workspaces/{id}/previews/{port}/grant.
type GrantAPI struct {
	// Proxy mints the grant. Nil answers 503: this server serves no previews.
	Proxy *Proxy
	// AuthOff is true only when the owner API runs without authentication (development). Otherwise
	// the caller must be a verified owner-scope principal, in observe mode too: observe refuses
	// nothing on its own, and a preview grant must never go to an anonymous caller.
	AuthOff bool
}

// GrantWorkspacePreview implements api.StrictServerInterface.
func (g *GrantAPI) GrantWorkspacePreview(ctx context.Context, r api.GrantWorkspacePreviewRequestObject) (api.GrantWorkspacePreviewResponseObject, error) {
	if !g.AuthOff {
		p, ok := auth.FromContext(ctx)
		if !ok {
			challenge := `Bearer realm="mainloop"`
			return api.GrantWorkspacePreview401JSONResponse{UnauthorizedJSONResponse: api.UnauthorizedJSONResponse{
				Body: api.HTTPError{Detail: "Authentication required"}, Headers: api.UnauthorizedResponseHeaders{WWWAuthenticate: &challenge},
			}}, nil
		}
		if p.Scope != auth.ScopeOwner {
			return api.GrantWorkspacePreview403JSONResponse{Detail: "Not permitted"}, nil
		}
	}
	if g.Proxy == nil {
		return nil, apierr.Unavailable("Previews are not configured")
	}
	var next string
	if r.Body != nil && r.Body.Path != nil {
		next = *r.Body.Path
	}
	url, exp, err := g.Proxy.Grant(ctx, r.WorkspaceId, r.Port, next)
	switch {
	case errors.Is(err, ErrNotPreviewable):
		return nil, apierr.NotFound("Workspace not found")
	case errors.Is(err, ErrPortNotDeclared):
		return nil, apierr.Invalid("Preview port is not declared")
	case errors.Is(err, ErrBadPath):
		return nil, apierr.Invalid("path must be a path on the preview host, starting with a single /")
	case err != nil:
		return nil, err
	}
	return api.GrantWorkspacePreview200JSONResponse{Url: url, ExpiresAt: exp}, nil
}
