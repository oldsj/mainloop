package attention_test

import (
	"context"
	"strings"
	"testing"
)

// The sweep reads the newest inbox:updated event every pass. Its index holds only those events, so
// the read is one probe whatever other events the owner has.
func TestLastPublishedUnreadUsesThePartialIndex(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `EXPLAIN SELECT (e.payload->>'unread_count')::int FROM events e
		WHERE e.user_id = $1 AND e.type = 'inbox:updated' ORDER BY e.id DESC LIMIT 1`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "events_user_inbox") {
		t.Fatalf("plan does not use events_user_inbox:\n%s", plan.String())
	}
}
