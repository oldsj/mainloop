package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/testdb"
)

// Migration 00970 marks the chat copies of report screenshots that existed before it, claimed
// or not, by their "<message id>:<screenshot id>" ids, and leaves pasted images unmarked.
func TestMigrationMarksExistingScreenshotCopies(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewEmpty(t)
	m := newMigrator(t, d.URL)
	if _, err := m.UpTo(ctx, 969); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	sess, msg, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	claimedShot, unclaimedShot := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO sessions (id, user_id, title, description, prompt) VALUES ($1, 'fictional-owner', 't', '', '')`, sess)
	exec(`INSERT INTO session_messages (id, session_id, role, content) VALUES ($1, $2, 'user', '[report from a fictional task]')`, msg, sess)
	rows := []struct {
		id, message, shot string
	}{
		{msg + ":" + claimedShot, msg, claimedShot},      // a copy its message claimed
		{other + ":" + unclaimedShot, "", unclaimedShot}, // a copy no message claimed (a failed send)
		{uuid.NewString(), msg, ""},                      // a pasted image, sent
		{uuid.NewString(), "", ""},                       // a pasted image, never sent
	}
	for _, r := range rows {
		var message *string
		if r.message != "" {
			message = &r.message
		}
		exec(`INSERT INTO message_images (id, session_id, message_id, media_type, data, size_bytes)
			VALUES ($1, $2, $3, 'image/png', '\x89504e47'::bytea, 4)`, r.id, sess, message)
	}

	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var got *string
		if err := d.Pool.QueryRow(ctx, `SELECT screenshot_id FROM message_images WHERE id = $1`, r.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (r.shot == "") || got != nil && *got != r.shot {
			t.Errorf("image %s: screenshot_id = %v, want %q", r.id, got, r.shot)
		}
	}
}
