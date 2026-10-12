package hitl

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
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
	return dbq.New(pool).RequestHITLScan(ctx, dbq.RequestHITLScanParams{KagentSessionID: kagentSessionID, TaskID: taskID})
}
