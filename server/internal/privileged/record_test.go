package privileged

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// recordVectorPath is the cross-language vector: the canonical record of vectorRow and its
// digest. The iOS app's tests hash the same bytes (ios/Packages/MainloopKit, ActionRecordTests)
// and bundle a copy as the fixture card's record. MAINLOOP_UPDATE_VECTORS=1 rewrites it.
const recordVectorPath = "testdata/canonical-record.json"

type recordVector struct {
	Record string `json:"record"`
	Digest string `json:"digest"`
}

// vectorRow is a fictional action whose facts exercise the canonical form: number text kept as
// written (1.50, 1e3), HTML characters left unescaped, U+2028 escaped, a character outside the
// BMP, a right-to-left override the phone must show visibly, and nested keys to sort.
func vectorRow() (dbq.PrivilegedAction, []byte) {
	task := "task-0001"
	return dbq.PrivilegedAction{
			ID: "pa-0001", Type: TypeRolloutRestart, Version: 1,
			Args:          json.RawMessage(`{"namespace":"shop","name":"web","kind":"Deployment"}`),
			RequesterRole: "worker", RequesterSessionID: "sess-0001", RequesterTaskID: &task,
			ExpiresAt: time.Date(2026, 11, 24, 18, 30, 0, 123456000, time.FixedZone("x", -5*3600)),
		}, []byte(`{"replicas": 3, "ready_replicas": 3, "ratio": 1.50, "big": 1e3,
		"image": "registry.example.test/shop/web:1.50",
		"note": "caf\u00e9 <b>&</b> \u2028 tab\there\nnext \ud83d\ude80 \u202eevil",
		"nested": {"z": [1, 2, {"b": true, "a": null}], "a": "x"}}`)
}

func TestTheCanonicalRecordVector(t *testing.T) {
	row, facts := vectorRow()
	rec, err := CanonicalRecord(row, facts)
	if err != nil {
		t.Fatal(err)
	}
	dig, err := Digest(row, facts)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rec)
	if hex.EncodeToString(sum[:]) != dig {
		t.Fatal("the digest is not the SHA-256 of the canonical record")
	}
	for _, want := range []string{`"ratio":1.50`, `"big":1e3`, `<b>&</b>`, `\u2028`, `"expires_at":"2026-11-24T23:30:00.123456Z"`,
		`"nested":{"a":"x","z":[1,2,{"a":null,"b":true}]}`} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("canonical record lacks %s:\n%s", want, rec)
		}
	}
	got := recordVector{Record: string(rec), Digest: dig}
	if os.Getenv("MAINLOOP_UPDATE_VECTORS") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recordVectorPath, append(asciiJSON(b), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(recordVectorPath)
	if err != nil {
		t.Fatal(err)
	}
	var want recordVector
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s is stale; rerun with MAINLOOP_UPDATE_VECTORS=1 and copy it to the iOS fixture", recordVectorPath)
	}
}

// asciiJSON escapes every non-ASCII character of JSON text as \uXXXX, so the committed vector
// holds no raw bidi controls. Non-ASCII only occurs inside strings, where the escape means the
// same.
func asciiJSON(b []byte) []byte {
	var out bytes.Buffer
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.Bytes()
}
