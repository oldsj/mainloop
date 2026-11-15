package transcripts

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

// TranscriptsAPI serves the transcript and search routes. The type name is unique because the app
// embeds every domain's API in one struct.
type TranscriptsAPI struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *TranscriptsAPI { return &TranscriptsAPI{Svc: svc} }

// GetSessionTranscript implements api.StrictServerInterface.
func (a *TranscriptsAPI) GetSessionTranscript(ctx context.Context, r api.GetSessionTranscriptRequestObject) (api.GetSessionTranscriptResponseObject, error) {
	req := ReadRequest{SessionID: r.SessionId, Live: true}
	if p := r.Params.After; p != nil {
		if *p < 0 {
			return nil, apierr.Invalid("after must not be negative")
		}
		req.After = *p
	}
	if p := r.Params.Before; p != nil {
		if *p < 1 {
			return nil, apierr.Invalid("before must be positive")
		}
		req.Before = *p
	}
	if p := r.Params.Latest; p != nil {
		req.Latest = *p
	}
	if n := btoi(req.After > 0) + btoi(req.Before > 0) + btoi(req.Latest); n > 1 {
		return nil, apierr.Invalid("after, before and latest exclude each other")
	}
	if p := r.Params.Limit; p != nil {
		if *p < 1 || *p > MaxReadLimit {
			return nil, apierr.Invalid("limit must be between 1 and 500")
		}
		req.Limit = *p
	}
	if p := r.Params.Live; p != nil {
		req.Live = *p
	}
	out, err := a.Svc.Read(ctx, req)
	if errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("Session not found")
	}
	if err != nil {
		return nil, err
	}
	return api.GetSessionTranscript200JSONResponse(out), nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SearchTranscripts implements api.StrictServerInterface.
func (a *TranscriptsAPI) SearchTranscripts(ctx context.Context, r api.SearchTranscriptsRequestObject) (api.SearchTranscriptsResponseObject, error) {
	p := r.Params
	if p.Q == "" || len(p.Q) > MaxQueryLen {
		return nil, apierr.Invalid("q must be 1 to 200 characters")
	}
	req := SearchRequest{Query: p.Q, ProjectID: p.ProjectId, SessionID: p.SessionId}
	if p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > MaxSearchLimit {
			return nil, apierr.Invalid("limit must be between 1 and 100")
		}
		req.Limit = *p.Limit
	}
	out, err := a.Svc.Search(ctx, req)
	if err != nil {
		return nil, err
	}
	return api.SearchTranscripts200JSONResponse(out), nil
}
