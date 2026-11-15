package dotfiles

import (
	"context"
	"sync"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// HeadTTL is how long a resolved ref is reused before GitHub is read again.
const HeadTTL = time.Minute

// Head is a session's dotfiles source as it is now: the source that applies, and the commit its ref
// names. Commit is empty when no source applies, or when the ref could not be resolved (Reason).
type Head struct {
	Repository string
	Ref        string
	Path       string
	Commit     string
	Reason     string
}

// Behind reports whether a refresh would give the session other dotfiles than p, the pin it holds
// (ok false when it holds none recorded): the source resolved, and p is from another repository or
// directory, or another commit. An unresolved or absent source is never behind.
func (h Head) Behind(p Pinned, ok bool) bool {
	if h.Commit == "" {
		return false
	}
	return !ok || p.Repository != h.Repository || p.Path != h.Path || p.Commit != h.Commit
}

type headEntry struct {
	commit, reason string
	at             time.Time
}

// headCache keeps resolved refs for HeadTTL, so showing a session does not read GitHub each time.
type headCache struct {
	mu      sync.Mutex
	entries map[Source]headEntry
}

// Head returns sessionID's source and the commit its ref names, reusing a resolve younger than
// HeadTTL. It reads GitHub with no transaction open, and writes nothing.
func (e *Exports) Head(ctx context.Context, sessionID string) (Head, error) {
	src, err := e.effectiveSource(ctx, dbq.New(e.Pool), sessionID)
	if err != nil || src.Repository == "" {
		return Head{}, err
	}
	h := Head{Repository: src.Repository, Ref: src.Ref, Path: src.Path}
	now := time.Now()
	e.heads.mu.Lock()
	cached, ok := e.heads.entries[src]
	e.heads.mu.Unlock()
	if ok && now.Sub(cached.at) < HeadTTL {
		h.Commit, h.Reason = cached.commit, cached.reason
		return h, nil
	}
	commit, unavailable := e.Resolve(ctx, src)
	entry := headEntry{commit: commit, at: now}
	if unavailable != nil {
		entry.reason = unavailable.Reason
	}
	e.heads.mu.Lock()
	if e.heads.entries == nil {
		e.heads.entries = map[Source]headEntry{}
	}
	e.heads.entries[src] = entry
	e.heads.mu.Unlock()
	h.Commit, h.Reason = entry.commit, entry.reason
	return h, nil
}
