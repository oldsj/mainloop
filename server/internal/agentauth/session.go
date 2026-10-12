package agentauth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// authorizationHeader is the header the MCP bearer travels in. kagent compares session credential
// headers byte for byte against lowercase (preparation refuses a session that spells one otherwise), so
// it is spelled lowercase.
const authorizationHeader = "authorization"

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
	return c.SessionCredentialsFor(ctx, bindingID, "", origin)
}

// SessionCredentialsFor is SessionCredentials for one native identity of the binding.
func (c *Credentials) SessionCredentialsFor(ctx context.Context, bindingID, identity, origin string) ([]*kagentapi.SessionCredential, error) {
	if c == nil {
		return nil, nil
	}
	issued, err := c.IssueFor(ctx, bindingID, dbstate.CredentialMCP, identity)
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

// ReplayCredentials returns the credential reference the binding's create carried, for a replay of
// that create. kagent refuses a replayed request id whose credentials differ from the original's,
// so a replay must send the same reference whatever became of the credential since (it may be
// revoked by now). The reference is a function of the binding alone: the Secret name is
// deterministic (a function of the binding and its identity). It is sent when the store is configured and the role and grant are a valid pair,
// the two conditions under which [Credentials.SessionCredentials] sent one. It reads nothing and
// publishes nothing.
func (c *Credentials) ReplayCredentials(bindingID, origin string, role dbstate.BindingRole, grant dbstate.MCPGrantKind) []*kagentapi.SessionCredential {
	return c.ReplayCredentialsFor(bindingID, "", origin, role, grant)
}

// ReplayCredentialsFor is ReplayCredentials for one native identity of the binding.
func (c *Credentials) ReplayCredentialsFor(bindingID, identity, origin string, role dbstate.BindingRole, grant dbstate.MCPGrantKind) []*kagentapi.SessionCredential {
	if c == nil || !ValidPair(role, grant) {
		return nil
	}
	if _, off := c.Secrets.(Unconfigured); off {
		return nil
	}
	return []*kagentapi.SessionCredential{reference(origin, c.SecretNameFor(dbstate.CredentialMCP, bindingID, identity))}
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
