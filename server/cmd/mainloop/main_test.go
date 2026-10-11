package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args       []string
		code       int
		stderrHas  string
		stdoutHas  string
		stderrOnly bool
	}{
		{[]string{"smoke"}, 2, "not implemented yet", "", true},
		{nil, 1, "usage:", "", true},
		{[]string{"bogus"}, 1, `unknown command "bogus"`, "", true},
		{[]string{"help"}, 0, "", "serve-api", false},
	}
	for _, tt := range tests {
		var out, errb bytes.Buffer
		if got := run(tt.args, &out, &errb); got != tt.code {
			t.Errorf("run(%v) = %d, want %d", tt.args, got, tt.code)
		}
		if !strings.Contains(errb.String(), tt.stderrHas) || !strings.Contains(out.String(), tt.stdoutHas) {
			t.Errorf("run(%v): stdout %q stderr %q", tt.args, out.String(), errb.String())
		}
		if tt.stderrOnly && out.Len() != 0 {
			t.Errorf("run(%v) wrote to stdout: %q", tt.args, out.String())
		}
	}
}

func TestServeRequiresAgentTokenKey(t *testing.T) {
	t.Setenv("AGENT_TOKEN_KEY", "")
	var out, errb bytes.Buffer
	if got := run([]string{"serve-api"}, &out, &errb); got != 1 {
		t.Fatalf("exit = %d", got)
	}
	if !strings.Contains(out.String(), "AGENT_TOKEN_KEY") {
		t.Errorf("log does not name the missing variable: %s", out.String())
	}
}
