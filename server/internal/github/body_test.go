package github

import (
	"encoding/json"
	"testing"
)

func TestPullBodyPreservesValidTextAndRejectsInvalidUnicode(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want  string
		available, valid bool
	}{
		{"empty", `""`, "", true, true},
		{"null", `null`, "", false, true},
		{"supported", `"é\n\t\r�"`, "é\n\t\r�", true, true},
		{"surrogate_pair", `"\ud83d\ude00"`, "😀", true, true},
		{"literal_escape", `"\\ud800"`, `\ud800`, true, true},
		{"nul", `"\u0000"`, "\x00", true, true},
		{"high_surrogate", `"\ud800"`, "", false, false},
		{"low_surrogate", `"\udc00"`, "", false, false},
		{"unpaired_high", `"\ud800\u0041"`, "", false, false},
		{"invalid_utf8", "\"\xff\"", "", false, false},
		{"non_string", `42`, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p pullJSON
			err := json.Unmarshal([]byte(`{"body":`+tc.raw+`}`), &p)
			if (err == nil) != tc.valid {
				t.Fatalf("decode = %v", err)
			}
			got := p.info()
			if tc.valid && (got.BodyAvailable != tc.available || got.Body != tc.want) {
				t.Fatalf("body = %q, available = %v", got.Body, got.BodyAvailable)
			}
		})
	}
}
