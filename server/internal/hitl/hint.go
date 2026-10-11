package hitl

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RequestScan hints that a session's task just parked for input, so the observer should list its
// tasks on its next sweep instead of waiting out ScanEvery. The hint is a "due now" mark on the
// session's hitl_sessions row (scanned_at back to NULL, which the scan order already treats as
// most urgent). It is a database mark, not an in-process channel, so it works when the delivery
// loop and the observer run in different processes, and a burst of parks sets the same mark: it
// coalesces. It starts no scan: the observer sweep still holds its lease and runs sessions one at a
// time, a session in failure backoff keeps its delay (next_scan_at is untouched), and a task that
// already has a live request is left alone, since the refresh loop keeps that one honest. A hint
// that is lost (the session has no row yet, which a new row scans first anyway, or a scan in flight
// finished after the park and stamped scanned_at) only falls back to the periodic scan.
func RequestScan(ctx context.Context, pool *pgxpool.Pool, kagentSessionID, taskID string) error {
	_, err := pool.Exec(ctx, `
UPDATE hitl_sessions hs
SET scanned_at = NULL, updated_at = now()
WHERE hs.kagent_session_id = $1
  AND hs.scanned_at IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM hitl_requests r
      WHERE r.kagent_session_id = hs.kagent_session_id AND r.task_id = $2
        AND r.availability IN ('pending', 'stale')
  )`, kagentSessionID, taskID)
	return err
}
