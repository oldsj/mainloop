package approval

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/devices"
)

// The card's context keys (privileged.ContextActionID, ContextDigest, ContextType).
const (
	contextActionID = "privileged_action_id"
	contextType     = "privileged_action_type"
)

// Checker checks an Approve of a privileged action's card in serve-api, in the transaction of the
// answer. It implements attention.PrivilegedApprovals.
type Checker struct {
	OwnerID string
	// Audience is the instance's origin, which a token must be signed for (auth.Devices.Audience).
	Audience string
	// Pinned is the executor's pinned approval keys (MAINLOOP_PRIVILEGED_APPROVAL_KEYS). With any
	// pinned, an executor type is approved only from a phone whose key is pinned, so the
	// executor never gets an approval it would refuse.
	Pinned Pinned
	// ExecutorTypes are the types serve-executor runs.
	ExecutorTypes map[string]bool
	Now           func() time.Time
}

var _ attention.PrivilegedApprovals = (*Checker)(nil)

func forbidden(msg string) error { return fmt.Errorf("%w: %s", attention.ErrForbidden, msg) }

// CheckApprove implements attention.PrivilegedApprovals. A phone's Approve must carry a token its
// approval key signed for this card's action and digest, used once. The web app's Approve carries
// none, and is refused for an executor type while keys are pinned.
func (c *Checker) CheckApprove(ctx context.Context, tx pgx.Tx, card map[string]any, metadata []byte) error {
	actionID, _ := card[contextActionID].(string)
	digest, _ := card[attention.PrivilegedDigestKey].(string)
	typ, _ := card[contextType].(string)
	var meta map[string]any
	_ = json.Unmarshal(metadata, &meta) // Respond has checked it is JSON
	token, _ := meta[MetadataKey].(string)
	needsPinned := len(c.Pinned) > 0 && c.ExecutorTypes[typ]

	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind != auth.KindDevice {
		if token != "" {
			return forbidden("an approval token is only accepted with the phone's own answer")
		}
		if needsPinned {
			return forbidden("approve this on your iPhone: the executor runs only approvals signed with a pinned Touch ID key")
		}
		return nil
	}
	if token == "" {
		return forbidden("approving a privileged action on a phone needs its Touch ID signature")
	}
	q := dbq.New(tx)
	dev, err := q.GetOwnerDevice(ctx, p.Name)
	if err != nil {
		return err
	}
	if dev.OwnerID != c.OwnerID || dev.RevokedAt != nil {
		return forbidden("this device is not registered")
	}
	if dev.ApprovalKeyX == nil || dev.ApprovalKeyY == nil {
		return forbidden("this iPhone has no approval key: sign out and in again to approve privileged actions on it")
	}
	key, err := devices.PublicKey(*dev.ApprovalKeyX, *dev.ApprovalKeyY)
	if err != nil {
		return fmt.Errorf("device %s: stored approval key: %w", dev.ID, err)
	}
	claims, err := Parse(token, func(kid string) (*ecdsa.PublicKey, error) {
		if kid != p.Name {
			return nil, errors.New("signed by another device")
		}
		return key, nil
	}, c.Audience)
	if err == nil {
		err = claims.Current(c.now())
	}
	if err != nil {
		return forbidden(err.Error())
	}
	if actionID == "" || claims.ActionID != actionID || claims.Digest != digest {
		return forbidden("the approval was signed for another action or digest")
	}
	if needsPinned && !c.Pinned.Has(p.Name, key) {
		return forbidden("this iPhone's approval key is not pinned in the executor's configuration, so the executor would refuse the approval")
	}
	n, err := q.RecordPrivilegedApproval(ctx, dbq.RecordPrivilegedApprovalParams{
		ActionID: actionID, DeviceID: p.Name, Jti: claims.JTI, Token: token, SignedAt: claims.IssuedAt,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return forbidden("this approval token was already used")
	}
	return nil
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
