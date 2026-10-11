package config

import "os"

// TestDatabaseURL returns the disposable PostgreSQL server tests may use, or "" when unset.
// It lives here so that no other package reads the environment.
func TestDatabaseURL() string { return os.Getenv("MAINLOOP_TEST_DATABASE_URL") }
