package environments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// followEvery is how often the sweep resolves the watched tags.
const followEvery = time.Minute

// FollowSweep keeps every environment that opted in to following its watched tag on the newest
// image the tag points at. It is the only automatic default change; without the opt-in a watched
// tag stays metadata and the owner accepts versions explicitly.
func (s *Service) FollowSweep(owner string) reconcile.Sweep {
	return reconcile.Sweep{Name: "environment-follow", Every: followEvery, Run: func(ctx context.Context) error {
		return s.Follow(ctx, owner)
	}}
}

// Follow runs one pass over the owner's following environments. One environment's failure does not
// stop the others; the errors are joined.
func (s *Service) Follow(ctx context.Context, owner string) error {
	envs, err := dbq.New(s.Pool).ListFollowingDevEnvironments(ctx, owner)
	if err != nil {
		return err
	}
	var errs []error
	for _, env := range envs {
		if err := s.follow(ctx, env); err != nil {
			errs = append(errs, fmt.Errorf("environment %s: %w", env.ID, err))
		}
	}
	return errors.Join(errs...)
}

// follow resolves the environment's tag against its newest version. A changed image is appended as
// a new version and accepted as the default, at most once per FollowInterval; the first image is
// always accepted. A version the sweep would not be allowed to promote by hand is never promoted.
// A rollback is the project selecting an explicit older version, which follow never touches.
func (s *Service) follow(ctx context.Context, env dbq.DevEnvironment) error {
	if s.followWaiting(env) {
		return nil
	}
	latest, err := dbq.New(s.Pool).LatestEnvironmentVersion(ctx, env.ID)
	if err != nil {
		return err
	}
	// No transaction is open here: the registry can be slow.
	next, err := s.Validator.Refresh(ctx, env, latest, s.id())
	if err != nil {
		return err
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.LockDevEnvironment(ctx, env.ID)
		if err != nil {
			return err
		}
		if !cur.FollowWatchedTag || s.followWaiting(cur) {
			return nil
		}
		target, err := q.LatestEnvironmentVersion(ctx, env.ID)
		if err != nil {
			return err
		}
		accepted := false
		if target.PlatformManifestDigest == nil || *target.PlatformManifestDigest != *next.PlatformManifestDigest || defaultEligible(target) != nil {
			if target, err = q.InsertEnvironmentVersion(ctx, next); err != nil {
				return err
			}
			accepted = true
		}
		if err := defaultEligible(target); err != nil {
			return err
		}
		if cur.DefaultVersionID != nil && *cur.DefaultVersionID == target.ID {
			return nil
		}
		// Only a changed image starts the interval; setting the first default does not.
		params := dbq.SetDevEnvironmentFollowedParams{ID: env.ID, DefaultVersionID: &target.ID, LastFollowedAt: cur.LastFollowedAt}
		if accepted {
			now := s.now()
			params.LastFollowedAt = &now
		}
		_, err = q.SetDevEnvironmentFollowed(ctx, params)
		return err
	})
}

// followWaiting says whether the environment accepted an image less than FollowInterval ago.
func (s *Service) followWaiting(env dbq.DevEnvironment) bool {
	return env.LastFollowedAt != nil && s.now().Before(env.LastFollowedAt.Add(s.FollowInterval))
}
