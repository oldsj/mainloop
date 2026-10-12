package config

import "os"

// TestDatabaseURL returns the disposable PostgreSQL server tests may use, or "" when unset.
// It lives here so that no other package reads the environment.
func TestDatabaseURL() string { return os.Getenv("MAINLOOP_TEST_DATABASE_URL") }

// RequireTestDatabase reports whether a missing test database is a failure instead of a skip.
// CI sets CI=true and always provides the database.
func RequireTestDatabase() bool { return os.Getenv("CI") == "true" }
