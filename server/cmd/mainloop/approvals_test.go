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
	"github.com/oldsj/mainloop/server/internal/config"
)

// Given the approval settings both processes start with, an executor with no mode chosen, signed
// with no key pinned, and web with a key pinned each refuse to start; web with an executor warns
// loudly; and signed with a key starts quietly.
func TestCheckApprovalKeys(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pinned := approval.Pinned{"dev_00000000000000000000000001": &k.PublicKey}
	cases := []struct {
		name      string
		executor  bool
		mode      config.ApprovalsMode
		pinned    approval.Pinned
		err, warn bool
	}{
		{"executor, no mode", true, config.ApprovalsUnset, nil, true, false},
		{"executor, no mode, keys pinned", true, config.ApprovalsUnset, pinned, true, false},
		{"no executor, no mode", false, config.ApprovalsUnset, nil, false, false},
		{"signed, none pinned", true, config.ApprovalsSigned, nil, true, false},
		{"signed, none pinned, no executor", false, config.ApprovalsSigned, nil, true, false},
		{"signed", true, config.ApprovalsSigned, pinned, false, false},
		{"web with an executor", true, config.ApprovalsWeb, nil, false, true},
		{"web, no executor", false, config.ApprovalsWeb, nil, false, false},
		{"web, a key pinned", true, config.ApprovalsWeb, pinned, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := checkApprovalKeys(slog.New(slog.NewTextHandler(&out, nil)), c.executor, c.mode, c.pinned)
			if (err != nil) != c.err {
				t.Fatalf("err = %v", err)
			}
			if warned := strings.Contains(out.String(), "UNSIGNED PRIVILEGED APPROVALS"); warned != c.warn {
				t.Fatalf("warned %v: %s", warned, out.String())
			}
		})
	}
}
