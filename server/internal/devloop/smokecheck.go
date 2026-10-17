package devloop

import (
	"context"
	"io"

	"github.com/oldsj/mainloop/server/internal/smoke"
)

// SmokeCheck is one pass/fail step of dev_smoke's result.
type SmokeCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// SmokeRunner runs the scripted read-only smoke check against mainloop-dev.
type SmokeRunner interface {
	Run(ctx context.Context) (checks []SmokeCheck, failed bool, err error)
}

// ReadOnlySmoke is the real [SmokeRunner]: [smoke.ReadOnly] pointed at mainloop-dev's own
// Services, the same check `mainloop smoke --read-only` already runs by hand.
type ReadOnlySmoke struct {
	Options smoke.ReadOptions
}

func (r ReadOnlySmoke) Run(ctx context.Context) ([]SmokeCheck, bool, error) {
	var raw []smoke.Check
	smoke.ReadOnly(ctx, r.Options, func(c smoke.Check) { raw = append(raw, c) })
	failed := smoke.Summary(io.Discard, raw)
	checks := make([]SmokeCheck, 0, len(raw))
	for _, c := range raw {
		checks = append(checks, SmokeCheck{Name: c.Name, Status: string(c.Status), Detail: c.Detail})
	}
	return checks, failed, nil
}
