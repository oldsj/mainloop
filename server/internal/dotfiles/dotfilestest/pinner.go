// Package dotfilestest is a dotfiles pinner for the tests of the packages that create native
// Sessions: it records pins as the real one does, without reading GitHub.
package dotfilestest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
)

// Bundle is the fictional bundle a Ready pinner pins, and Commit the commit it names.
var (
	Bundle = []byte("fictional dotfiles bundle")
	Commit = strings.Repeat("f", 40)
)

// Digest is Bundle's digest.
func Digest() string {
	sum := sha256.Sum256(Bundle)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Call is one Pin call.
type Call struct {
	Session, Request, MCPOrigin string
	// CreatesBefore is what Creates returned when the pin ran.
	CreatesBefore int
}

// Pinner pins Bundle when Ready, else no dotfiles, and records each call. With Err set it fails
// before writing anything.
type Pinner struct {
	Pool    *pgxpool.Pool
	Ready   bool
	Err     error
	Creates func() int

	mu    sync.Mutex
	calls []Call
}

// Pin implements the sessions and workspaces pinner.
func (p *Pinner) Pin(ctx context.Context, sessionID, requestID, mcpOrigin string) (dotfiles.Pinned, error) {
	c := Call{Session: sessionID, Request: requestID, MCPOrigin: mcpOrigin}
	if p.Creates != nil {
		c.CreatesBefore = p.Creates()
	}
	p.mu.Lock()
	p.calls = append(p.calls, c)
	p.mu.Unlock()
	if p.Err != nil {
		return dotfiles.Pinned{}, p.Err
	}
	q := dbq.New(p.Pool)
	pin := dotfiles.Pinned{Status: dotfiles.StatusNone}
	if p.Ready {
		if err := q.InsertDotfilesBundle(ctx, dbq.InsertDotfilesBundleParams{Digest: Digest(), Content: Bundle}); err != nil {
			return dotfiles.Pinned{}, err
		}
		pin = dotfiles.Pinned{
			Repository: "example/dotfiles", Ref: "main", Path: "home/.agents",
			Commit: Commit, Digest: Digest(), Status: dotfiles.StatusReady,
		}
	}
	return dotfiles.Record(ctx, q, sessionID, requestID, mcpOrigin, pin)
}

// Calls returns the calls so far.
func (p *Pinner) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Call(nil), p.calls...)
}
