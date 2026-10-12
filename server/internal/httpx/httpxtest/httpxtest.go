// Package httpxtest checks what every listener's middleware chain logs.
package httpxtest

import (
	"encoding/json"
	"strings"
	"testing"
)

// RequireRequestID fails unless logs (JSON lines) holds at least one access line ("request") and
// every one carries a non-empty request_id. It returns the ids so a test can compare them with a
// response header.
func RequireRequestID(t testing.TB, logs string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != "request" {
			continue
		}
		id, _ := rec["request_id"].(string)
		if id == "" {
			t.Fatalf("access line has an empty request_id: %s", line)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		t.Fatalf("no access line in the log: %q", logs)
	}
	return ids
}
