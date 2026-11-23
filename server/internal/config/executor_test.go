package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
)

// serve-executor needs only its own settings: no agent token key, a 1Password token, and a
// namespace list that must be names.
func TestParseExecutor(t *testing.T) {
	e, err := config.ParseExecutor(map[string]string{
		"MAINLOOP_EXECUTOR_OP_TOKEN": "fictional", "MAINLOOP_EXECUTOR_SECRET_NAMESPACES": " widgets, , billing ",
		"DB_USER": "mainloop_executor",
	})
	if err != nil || !slices.Equal(e.SecretNamespaces, []string{"widgets", "billing"}) || e.DB.User != "mainloop_executor" {
		t.Fatalf("executor = %+v, %v", e, err)
	}
	if _, err := config.ParseExecutor(map[string]string{}); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_OP_TOKEN") {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := config.ParseExecutor(map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_SECRET_NAMESPACES": "Bad_NS"}); err == nil {
		t.Fatal("a bad namespace was accepted")
	}
}
