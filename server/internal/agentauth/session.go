package agentauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// authorizationHeader is the header the MCP bearer travels in.
const authorizationHeader = "Authorization"

// SessionCredentials publishes the binding's MCP credential and returns the reference a kagent
// CreateSession carries, so the session's agent can reach the MCP server at origin. It runs before
// the create and outside any transaction, and it is idempotent, so a replayed create finds the
// credential it already published.
//
// A nil receiver, and a process outside a cluster ([Unconfigured]), return no credentials and no
// error: sessions are still created, only without MCP tools. The latter is logged. Errors the
// caller should treat as "try again later" are [ErrCleanupPending] and [ErrRevoked]; the session
// having ended is [ErrBindingEnded].
func (c *Credentials) SessionCredentials(ctx context.Context, bindingID, origin string) ([]*kagentapi.SessionCredential, error) {
	if c == nil {
		return nil, nil
	}
	issued, err := c.Issue(ctx, bindingID, dbstate.CredentialMCP)
	if errors.Is(err, ErrNotConfigured) {
		c.log().WarnContext(ctx, "session is created without an MCP credential: Kubernetes is not configured",
			"binding", bindingID)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []*kagentapi.SessionCredential{reference(origin, issued.SecretName)}, nil
}

// SessionReference returns the credential reference of the binding's live published MCP
// credential, or nil when it has none. It publishes nothing: a replayed create that only looks for
// a Session an earlier attempt made sends the same request as the original, without making a
// credential for a binding that has ended.
func (c *Credentials) SessionReference(ctx context.Context, bindingID, origin string) ([]*kagentapi.SessionCredential, error) {
	if c == nil {
		return nil, nil
	}
	row, err := dbq.New(c.Pool).GetLiveCredential(ctx, dbq.GetLiveCredentialParams{BindingID: bindingID, Kind: dbstate.CredentialMCP})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentauth: read credential: %w", err)
	}
	if row.State != dbstate.CredentialPublished {
		return nil, nil
	}
	return []*kagentapi.SessionCredential{reference(origin, row.SecretName)}, nil
}

func reference(origin, secretName string) *kagentapi.SessionCredential {
	return &kagentapi.SessionCredential{
		Origin: origin, Header: authorizationHeader,
		SecretRef: &kagentapi.SecretKeyReference{Name: secretName, Key: k8ssecrets.DataKey},
	}
}

func (c *Credentials) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
