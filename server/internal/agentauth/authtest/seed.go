// Package authtest seeds agents and their credentials for tests of the MCP server and its
// callers. It uses the real credential code with a fake Kubernetes cluster.
package authtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets/secretstest"
)

// Key is the AGENT_TOKEN_KEY of the test credentials.
var Key = []byte("test-agent-token-key-0123456789ab")

// Env is a database with a fake cluster and the credential service over both.
type Env struct {
	Pool    *pgxpool.Pool
	Cluster *secretstest.Client
	Creds   *agentauth.Credentials
}

// Namespace is the namespace of the fake cluster.
const Namespace = "kagent"

// NewEnv wires the credential service to pool and an empty fake cluster.
func NewEnv(pool *pgxpool.Pool) *Env {
	cl := secretstest.New()
	return &Env{
		Pool: pool, Cluster: cl,
		Creds: &agentauth.Credentials{
			Pool: pool, Secrets: k8ssecrets.New(cl, Namespace), Key: Key, Prefix: "mainloop",
		},
	}
}

// Agent describes an agent to seed.
type Agent struct {
	Role   dbstate.BindingRole
	Grant  dbstate.MCPGrantKind
	Status dbstate.SessionStatus // default active
	User   string                // default "owner"
	// Archived archives the session after seeding.
	Archived bool
	// Parent is the parent session id of a task agent.
	Parent string
}

// Seeded is a seeded agent with its bearer.
type Seeded struct {
	SessionID    string
	UserID       string
	CredentialID string
	Token        string
}

// Seed creates a session and binding and, when the pair is valid, publishes the MCP credential.
func (e *Env) Seed(t testing.TB, a Agent) Seeded {
	t.Helper()
	ctx := context.Background()
	if a.Status == "" {
		a.Status = dbstate.SessionActive
	}
	if a.User == "" {
		a.User = "owner"
	}
	q := dbq.New(e.Pool)
	id := uuid.NewString()
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
		ID: id, UserID: a.User, Title: "t", Description: "d", Prompt: "p", BaseBranch: "main",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	var parent *string
	if a.Parent != "" {
		parent = &a.Parent
	}
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: id, Kind: "claude", Role: a.Role, MCPGrantKind: a.Grant, ParentSessionID: parent,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE sessions SET status = $2 WHERE id = $1`, id, string(a.Status)); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	if a.Archived {
		if _, err := q.ArchiveSession(ctx, id); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
	}
	out := Seeded{SessionID: id, UserID: a.User}
	if agentauth.ValidPair(a.Role, a.Grant) {
		issued, err := e.Creds.Issue(ctx, id, dbstate.CredentialMCP)
		if err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		out.CredentialID = issued.CredentialID
		out.Token = agentauth.Token(Key, issued.CredentialID)
	}
	return out
}
