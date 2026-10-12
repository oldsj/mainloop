package health

import (
	"context"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

const smokeLimit = 100

// Smoke returns the sanitized ledger facts an operator smoke run checks: which tasks hold
// capacity, the open deliveries and, for a branch, its pushes. Grants and message text stay out.
// It only reads, in one snapshot.
func (s *Service) Smoke(ctx context.Context, projectID string, branch *string) (api.SmokeObservations, error) {
	out := api.SmokeObservations{
		CapacityHolders: []string{}, Deliveries: []api.SmokeDelivery{}, Pushes: []api.SmokePush{},
		ParentCapacity: s.MaxChildrenPerParent,
	}
	err := s.readOnly(ctx, func(q *dbq.Queries) error {
		ok, err := q.HealthOwnsProject(ctx, dbq.HealthOwnsProjectParams{ID: projectID, UserID: s.OwnerID})
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if out.CapacityHolders, err = q.ListSmokeCapacityHolders(ctx, s.OwnerID); err != nil {
			return err
		}
		if out.CapacityHolders == nil {
			out.CapacityHolders = []string{}
		}
		held, err := q.CountSmokeHeldAttempts(ctx)
		if err != nil {
			return err
		}
		out.GlobalCapacityAvailable = int(held) < s.MaxChildrenGlobal

		deliveries, err := q.ListSmokeDeliveries(ctx, dbq.ListSmokeDeliveriesParams{UserID: s.OwnerID, ProjectID: &projectID})
		if err != nil {
			return err
		}
		out.DeliveriesTruncated = len(deliveries) > smokeLimit
		out.DeliveriesBusy = len(deliveries) > 0
		for _, d := range deliveries[:min(len(deliveries), smokeLimit)] {
			out.Deliveries = append(out.Deliveries, api.SmokeDelivery{MessageId: d.MessageID, State: string(d.State)})
		}

		if branch == nil || *branch == "" {
			return nil
		}
		pushes, err := q.ListSmokePushes(ctx, dbq.ListSmokePushesParams{ID: projectID, UserID: s.OwnerID, Branch: *branch})
		if err != nil {
			return err
		}
		out.PushesTruncated = len(pushes) > smokeLimit
		for _, p := range pushes[:min(len(pushes), smokeLimit)] {
			out.Pushes = append(out.Pushes, api.SmokePush{RequestId: p.RequestID, State: string(p.State), Branch: p.Branch})
		}
		out.PushConfirmed, err = q.SmokePushConfirmed(ctx, dbq.SmokePushConfirmedParams{ID: projectID, UserID: s.OwnerID, Branch: *branch})
		return err
	})
	if err != nil {
		return api.SmokeObservations{}, err
	}
	return out, nil
}
