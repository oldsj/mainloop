package agentauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// SecretStore is the Kubernetes side of a credential. [k8ssecrets.Store] implements it.
type SecretStore interface {
	// Create publishes the immutable Secret and returns its UID. It succeeds if a Secret with the
	// same contents already holds the name.
	Create(ctx context.Context, name, purpose, bearer string) (uid string, err error)
	// Lookup returns the UID of the Secret with this name.
	Lookup(ctx context.Context, name string) (uid string, found bool, err error)
	// Delete removes the Secret only if it still has this UID.
	Delete(ctx context.Context, name, uid string) error
}

var (
	// ErrInvalidGrant means the binding's role and grant are not a valid pair, so no MCP
	// credential may be issued for it.
	ErrInvalidGrant = errors.New("agentauth: binding role and grant are not a valid pair")
	// ErrCleanupPending means the binding's previous Secret is still owed a delete, so the name is
	// not free yet. Try again after cleanup.
	ErrCleanupPending = errors.New("agentauth: previous credential Secret is still being cleaned up")
	// ErrRevoked means the credential was revoked while it was being published. The Secret this
	// call created has been deleted again.
	ErrRevoked = errors.New("agentauth: credential was revoked while being published")
	// ErrBindingEnded means the binding's session is archived or finished, so it can no longer
	// act and no credential is issued for it.
	ErrBindingEnded = errors.New("agentauth: the binding's session has ended")
)

// Credentials issues and revokes the bearer credentials of agent bindings.
type Credentials struct {
	Pool    *pgxpool.Pool
	Secrets SecretStore
	// Key is AGENT_TOKEN_KEY.
	Key []byte
	// Prefix starts every Secret name (the Mainloop namespace).
	Prefix string
	Log    *slog.Logger
}

// Issued is a published credential.
type Issued struct {
	CredentialID string
	SecretName   string
	SecretUID    string
}

// SecretName is the name of the Secret for a binding's credential of a kind.
func (c *Credentials) SecretName(kind dbstate.CredentialKind, bindingID string) string {
	switch kind {
	case dbstate.CredentialGitRead:
		return c.Prefix + "-git-read-" + bindingID
	case dbstate.CredentialGitPush:
		return c.Prefix + "-git-push-" + bindingID
	}
	return c.Prefix + "-mcp-" + bindingID
}

func purpose(kind dbstate.CredentialKind) string {
	switch kind {
	case dbstate.CredentialGitRead:
		return "git-read"
	case dbstate.CredentialGitPush:
		return "git-push"
	}
	return "mcp"
}

// Issue publishes the binding's credential of a kind. It is idempotent: a published credential
// is returned as it is, and one left planned by an interrupted publish is resumed. The database
// holds the intent first; the Kubernetes call runs with no transaction open.
func (c *Credentials) Issue(ctx context.Context, bindingID string, kind dbstate.CredentialKind) (Issued, error) {
	if _, off := c.Secrets.(Unconfigured); off {
		return Issued{}, ErrNotConfigured // before any intent row is written
	}
	row, err := c.plan(ctx, bindingID, kind)
	if err != nil {
		return Issued{}, err
	}
	if row.State == dbstate.CredentialPublished {
		return Issued{CredentialID: row.ID, SecretName: row.SecretName, SecretUID: deref(row.SecretUID)}, nil
	}
	token := Token(c.Key, row.ID)
	cctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	uid, err := c.Secrets.Create(cctx, row.SecretName, purpose(kind), token)
	if err != nil {
		return Issued{}, fmt.Errorf("agentauth: publish %s: %w", row.SecretName, err)
	}
	if _, err := dbq.New(c.Pool).MarkCredentialPublished(ctx, dbq.MarkCredentialPublishedParams{ID: row.ID, SecretUID: &uid}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not planned any more: a concurrent Issue published it, or it was revoked.
			cur, gerr := dbq.New(c.Pool).GetCredential(ctx, row.ID)
			if gerr != nil {
				// The row's state is unknown, so the Secret is left for the cleanup and stray sweeps.
				return Issued{}, fmt.Errorf("agentauth: read credential %s after a publish miss: %w", row.ID, gerr)
			}
			if cur.State == dbstate.CredentialPublished && deref(cur.SecretUID) == uid {
				return Issued{CredentialID: row.ID, SecretName: row.SecretName, SecretUID: uid}, nil
			}
			if cur.State != dbstate.CredentialRevoked && cur.State != dbstate.CredentialDeleted {
				return Issued{}, fmt.Errorf("agentauth: credential %s is %s after a publish miss", row.ID, cur.State)
			}
			// Revoked: cleanup may already have marked the row deleted without seeing this
			// Secret, so nothing else will remove it. The UID precondition keeps a newer Secret
			// under the same name safe.
			if derr := c.Secrets.Delete(ctx, row.SecretName, uid); derr != nil {
				return Issued{}, fmt.Errorf("agentauth: remove the Secret of revoked credential %s: %w", row.ID, errors.Join(ErrRevoked, derr))
			}
			return Issued{}, ErrRevoked
		}
		return Issued{}, fmt.Errorf("agentauth: record publish: %w", err)
	}
	return Issued{CredentialID: row.ID, SecretName: row.SecretName, SecretUID: uid}, nil
}

func (c *Credentials) plan(ctx context.Context, bindingID string, kind dbstate.CredentialKind) (dbq.Credential, error) {
	var row dbq.Credential
	err := db.InTx(ctx, c.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		b, err := q.GetAgentBinding(ctx, bindingID)
		if err != nil {
			return fmt.Errorf("agentauth: read binding: %w", err)
		}
		if kind == dbstate.CredentialMCP && !ValidPair(b.Role, b.MCPGrantKind) {
			return ErrInvalidGrant
		}
		// The session row is locked FOR SHARE until this transaction commits: whatever ends the
		// session updates that row, so it either waits for this plan (and then revokes it in its own
		// transaction) or has committed already and is seen here.
		sess, err := q.LockSessionForCredential(ctx, bindingID)
		if err != nil {
			return fmt.Errorf("agentauth: read session: %w", err)
		}
		if sess.ArchivedAt != nil || stale(sess.Status) {
			return ErrBindingEnded
		}
		if live, err := q.GetLiveCredential(ctx, dbq.GetLiveCredentialParams{BindingID: bindingID, Kind: kind}); err == nil {
			row = live
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("agentauth: read credential: %w", err)
		}
		id := uuid.NewString()
		row, err = q.CreateCredential(ctx, dbq.CreateCredentialParams{
			ID: id, Kind: kind, BindingID: bindingID,
			TokenHash: HashToken(Token(c.Key, id)), SecretName: c.SecretName(kind, bindingID),
		})
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.ConstraintName == "credentials_secret_name" {
			return ErrCleanupPending
		}
		if err != nil {
			return fmt.Errorf("agentauth: plan credential: %w", err)
		}
		return nil
	})
	return row, err
}

// Revoke revokes the live credentials of one binding. Only that binding's rows change; the
// cleanup loop deletes their Secrets. It returns how many credentials were revoked.
func (c *Credentials) Revoke(ctx context.Context, bindingID string) (int, error) {
	return RevokeInTx(ctx, dbq.New(c.Pool), bindingID)
}

// RevokeInTx revokes the live credentials of one binding with the given queries, so a caller can
// do it in the same transaction as the state change that ends the binding (pass dbq.New(tx)).
// It touches no Kubernetes object; the cleanup loop deletes the Secrets after the commit.
func RevokeInTx(ctx context.Context, q *dbq.Queries, bindingID string) (int, error) {
	rows, err := q.RevokeBindingCredentials(ctx, bindingID)
	if err != nil {
		return 0, fmt.Errorf("agentauth: revoke credentials of %s: %w", bindingID, err)
	}
	return len(rows), nil
}

// ErrNotConfigured is what [Unconfigured] returns from Issue: this process has no Kubernetes to
// publish a Secret to.
var ErrNotConfigured = errors.New("agentauth: Kubernetes not configured")

// Unconfigured is the [SecretStore] of a process outside a cluster. Every call fails with
// [ErrNotConfigured]; revokes still record their intent in the database.
type Unconfigured struct{}

// Create always fails.
func (Unconfigured) Create(context.Context, string, string, string) (string, error) {
	return "", ErrNotConfigured
}

// Lookup always fails.
func (Unconfigured) Lookup(context.Context, string) (string, bool, error) {
	return "", false, ErrNotConfigured
}

// Delete always fails.
func (Unconfigured) Delete(context.Context, string, string) error { return ErrNotConfigured }

// CleanupTable is the intent table the cleanup loop drives: revoked credentials.
func CleanupTable() reconcile.Table {
	return reconcile.NewTable("credentials", string(dbstate.CredentialRevoked))
}

// createTimeout bounds the Secret create inside Issue. Cleanup relies on it: a revoked credential
// with no recorded UID may still have a create in flight until this long after its row was made.
const createTimeout = 30 * time.Second

// lookupGrace is how long past a credential row's creation a missing Secret may still appear.
const lookupGrace = createTimeout + 15*time.Second

// stalePlanned is how long a planned credential may wait for its publish before it is revoked
// and its Secret looked up by name.
const stalePlanned = 10 * time.Minute

// Register adds the cleanup loop and the sweep that revokes stranded credentials.
func (c *Credentials) Register(r *reconcile.Runner) {
	r.Add(reconcile.Loop{
		Name: "credential-cleanup", Table: CleanupTable(), Handle: c.cleanup,
		Backoff: reconcile.Backoff{Base: 2 * time.Second, Max: time.Minute, Jitter: 0.2},
	})
	r.AddSweep(reconcile.Sweep{Name: "credential-strays", Every: time.Minute, Run: c.RevokeStrays})
}

// RevokeStrays revokes credentials that nothing else will: a planned row whose publish never
// finished, a live row whose binding is gone, and a live row whose session has ended.
func (c *Credentials) RevokeStrays(ctx context.Context) error {
	q := dbq.New(c.Pool)
	age := stalePlanned.Seconds()
	planned, err := q.RevokeStalePlannedCredentials(ctx, age)
	if err != nil {
		return fmt.Errorf("agentauth: revoke stale planned credentials: %w", err)
	}
	orphans, err := q.RevokeOrphanedCredentials(ctx, age)
	if err != nil {
		return fmt.Errorf("agentauth: revoke orphaned credentials: %w", err)
	}
	ended, err := q.RevokeEndedSessionCredentials(ctx)
	if err != nil {
		return fmt.Errorf("agentauth: revoke credentials of ended sessions: %w", err)
	}
	if n := len(planned) + len(orphans) + len(ended); n > 0 && c.Log != nil {
		c.Log.InfoContext(ctx, "revoked stray credentials", "stale_planned", len(planned), "orphaned", len(orphans), "ended_sessions", len(ended))
	}
	return nil
}

// cleanup deletes the Secret of one revoked credential. It runs with no transaction open. A
// credential that was revoked before its UID was recorded has its Secret looked up by name; the
// delete always carries the UID, so a newer Secret under the same name is never touched.
func (c *Credentials) cleanup(ctx context.Context, claim reconcile.Claim) (reconcile.Outcome, error) {
	row, err := dbq.New(c.Pool).GetCredential(ctx, claim.ID)
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("agentauth: read credential %s: %w", claim.ID, err)
	}
	if row.State != dbstate.CredentialRevoked {
		return reconcile.Done(string(row.State)), nil
	}
	uid := deref(row.SecretUID)
	if uid == "" {
		found := false
		if uid, found, err = c.Secrets.Lookup(ctx, row.SecretName); err != nil {
			return reconcile.Retry(), err
		}
		if !found {
			// An Issue whose create is still in flight may yet land the Secret; wait out its deadline
			// before calling the name free.
			if wait := time.Until(row.CreatedAt.Add(lookupGrace)); wait > 0 {
				return reconcile.RetryAfter(min(wait, time.Minute)), nil
			}
			return reconcile.Done(string(dbstate.CredentialDeleted)), nil
		}
	}
	if err := c.Secrets.Delete(ctx, row.SecretName, uid); err != nil {
		return reconcile.Retry(), err
	}
	return reconcile.Done(string(dbstate.CredentialDeleted)), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
