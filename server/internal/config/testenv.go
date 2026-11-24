package config

import "os"

// TestDatabaseURL returns the disposable PostgreSQL server tests may use, or "" when unset.
// It lives here so that no other package reads the environment.
func TestDatabaseURL() string { return os.Getenv("MAINLOOP_TEST_DATABASE_URL") }

// RequireTestDatabase reports whether a missing test database is a failure instead of a skip.
// CI sets CI=true and always provides the database.
func RequireTestDatabase() bool { return os.Getenv("CI") == "true" }

// TestChrome names the Chrome binary the screenshot fence tests drive; empty lets them look for one.
func TestChrome() string { return os.Getenv("MAINLOOP_TEST_CHROME") }

// TestKubectl is the kubectl binary command.kubectl's tests run, the one the prod image pins; CI
// installs it. Empty means kubectl on PATH, when it is that release.
func TestKubectl() string { return os.Getenv("MAINLOOP_TEST_KUBECTL") }
