package main

import (
	"bytes"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// useDatabase points the DB_* settings at db, so the command under test migrates it.
func useDatabase(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	t.Setenv("DB_HOST", u.Hostname())
	t.Setenv("DB_PORT", strconv.Itoa(port))
	t.Setenv("DB_NAME", strings.TrimPrefix(u.Path, "/"))
	t.Setenv("DB_USER", u.User.Username())
	t.Setenv("DB_PASSWORD", pw)
}

func TestMigrateUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"up", "extra"}, {"sideways"}} {
		var out, errb bytes.Buffer
		if got := runMigrate(args, &out, &errb); got != exitFailure {
			t.Errorf("runMigrate(%v) = %d, want %d", args, got, exitFailure)
		}
		if !strings.Contains(errb.String(), "usage:") {
			t.Errorf("runMigrate(%v) stderr %q", args, errb.String())
		}
	}
}

func TestMigrateResetRefusedWithoutFlag(t *testing.T) {
	d := testdb.New(t)
	useDatabase(t, d.URL)
	t.Setenv("MAINLOOP_ALLOW_RESET", "")

	var out, errb bytes.Buffer
	if got := runMigrate([]string{"reset"}, &out, &errb); got != exitFailure {
		t.Fatalf("reset = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(errb.String(), "MAINLOOP_ALLOW_RESET") {
		t.Errorf("stderr %q", errb.String())
	}
	// The refusal must come before any change: the data is still there.
	if _, err := d.Pool.Exec(t.Context(), `INSERT INTO topics (id, user_id, name) VALUES ('t', 'u', 'n')`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if got := runMigrate([]string{"reset"}, &out, &errb); got != exitFailure {
		t.Fatalf("second reset = %d", got)
	}
	var n int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM topics`).Scan(&n); err != nil || n != 1 {
		t.Errorf("topics = %d, err = %v", n, err)
	}
}

func TestMigrateUpStatusReset(t *testing.T) {
	d := testdb.NewEmpty(t)
	useDatabase(t, d.URL)

	var out, errb bytes.Buffer
	if got := runMigrate([]string{"status"}, &out, &errb); got != 0 || !strings.Contains(out.String(), "pending") {
		t.Fatalf("status before up = %d, out %q err %q", got, out.String(), errb.String())
	}
	out.Reset()
	if got := runMigrate([]string{"up"}, &out, &errb); got != 0 || !strings.Contains(out.String(), "applied ") {
		t.Fatalf("up = %d, out %q err %q", got, out.String(), errb.String())
	}
	out.Reset()
	if got := runMigrate([]string{"status"}, &out, &errb); got != 0 || !strings.Contains(out.String(), "00010_core.sql") || strings.Contains(out.String(), "pending") {
		t.Fatalf("status after up = %d, out %q err %q", got, out.String(), errb.String())
	}

	t.Setenv("MAINLOOP_ALLOW_RESET", "1")
	out.Reset()
	if got := runMigrate([]string{"reset"}, &out, &errb); got != 0 {
		t.Fatalf("reset = %d, out %q err %q", got, out.String(), errb.String())
	}
}
