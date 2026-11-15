package dotfiles_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/github"
)

// Head names the session's source and the commit its ref names now, reading GitHub once per
// source within HeadTTL; no source reads nothing.
func TestHeadResolvesTheSourceOncePerTTL(t *testing.T) {
	env := newPinEnv(t)
	ctx := context.Background()
	h, err := env.e.Head(ctx, "main")
	if err != nil || h != (dotfiles.Head{}) || env.refs.calls != 0 {
		t.Fatalf("without a source: %+v, %v, %d reads", h, err, env.refs.calls)
	}
	env.setOwner(t, "example/dotfiles", "main", "home/.agents")
	want := dotfiles.Head{Repository: "example/dotfiles", Ref: "main", Path: "home/.agents", Commit: env.refs.sha}
	for range 3 {
		if h, err := env.e.Head(ctx, "main"); err != nil || h != want {
			t.Fatalf("head = %+v, %v; want %+v", h, err, want)
		}
	}
	// The task's project has no source of its own, so it shares the owner's, and the read.
	if h, err := env.e.Head(ctx, "task"); err != nil || h != want {
		t.Fatalf("task head = %+v, %v", h, err)
	}
	if env.refs.calls != 1 {
		t.Fatalf("%d GitHub reads, want 1", env.refs.calls)
	}
}

func TestHeadReportsARefThatDoesNotResolve(t *testing.T) {
	env := newPinEnv(t)
	env.refs.err = github.ErrNotFound
	env.setOwner(t, "example/dotfiles", "gone", "")
	h, err := env.e.Head(context.Background(), "main")
	if err != nil || h.Commit != "" || h.Reason != dotfiles.ReasonRefNotFound || h.Repository != "example/dotfiles" {
		t.Fatalf("head = %+v, %v", h, err)
	}
}

func TestBehind(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	head := dotfiles.Head{Repository: "example/dotfiles", Ref: "main", Path: "home/.agents", Commit: commit}
	held := dotfiles.Pinned{Repository: "example/dotfiles", Ref: "main", Path: "home/.agents", Commit: commit, Status: dotfiles.StatusReady}
	for _, tc := range []struct {
		name   string
		head   dotfiles.Head
		pin    dotfiles.Pinned
		pinned bool
		want   bool
	}{
		{"same commit", head, held, true, false},
		{"another ref naming the same commit", head, dotfiles.Pinned{Repository: held.Repository, Path: held.Path, Commit: commit}, true, false},
		{"a newer commit", head, dotfiles.Pinned{Repository: held.Repository, Path: held.Path, Commit: other}, true, true},
		{"another directory", head, dotfiles.Pinned{Repository: held.Repository, Path: "other", Commit: commit}, true, true},
		{"held none when created", head, dotfiles.Pinned{Status: dotfiles.StatusNone}, true, true},
		{"no pin recorded", head, dotfiles.Pinned{}, false, true},
		{"the ref does not resolve", dotfiles.Head{Repository: "example/dotfiles", Reason: dotfiles.ReasonRefNotFound}, held, true, false},
		{"no source", dotfiles.Head{}, held, true, false},
	} {
		if got := tc.head.Behind(tc.pin, tc.pinned); got != tc.want {
			t.Errorf("%s: behind = %v, want %v", tc.name, got, tc.want)
		}
	}
}
