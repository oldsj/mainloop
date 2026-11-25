package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"log/slog"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/approval"
)

// Given the approval-key settings both processes start with, required with no key pinned refuses
// to start, no key pinned with an executor warns loudly, and pinned keys start quietly.
func TestCheckApprovalKeys(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pinned := approval.Pinned{"dev_00000000000000000000000001": &k.PublicKey}
	cases := []struct {
		name               string
		executor, required bool
		pinned             approval.Pinned
		err, warn          bool
	}{
		{"required, none pinned", true, true, nil, true, false},
		{"required, none pinned, no executor", false, true, nil, true, false},
		{"none pinned with an executor", true, false, nil, false, true},
		{"none pinned, no executor", false, false, nil, false, false},
		{"pinned and required", true, true, pinned, false, false},
		{"pinned", true, false, pinned, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := checkApprovalKeys(slog.New(slog.NewTextHandler(&out, nil)), c.executor, c.required, c.pinned)
			if (err != nil) != c.err {
				t.Fatalf("err = %v", err)
			}
			if warned := strings.Contains(out.String(), "UNSIGNED PRIVILEGED APPROVALS"); warned != c.warn {
				t.Fatalf("warned %v: %s", warned, out.String())
			}
		})
	}
}
