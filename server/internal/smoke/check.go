// Package smoke is the operator's check of a running Mainloop: read-only probes that cost no quota,
// and a budgeted live run that drives one real task from chat to merge. It only reads, except for the
// one chat submission per live run. Every check is one line, `ok|fail|skip name detail`.
package smoke

import (
	"fmt"
	"io"
	"strings"
)

// Status is the outcome of one check.
type Status string

const (
	OK   Status = "ok"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one named result. Detail never carries a secret: the probes read sanitized facts only.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// String formats the check as one output line.
func (c Check) String() string {
	if c.Detail == "" {
		return fmt.Sprintf("%s %s", c.Status, c.Name)
	}
	return fmt.Sprintf("%s %s %s", c.Status, c.Name, c.Detail)
}

func okCheck(name, detail string) Check {
	return Check{Name: name, Status: OK, Detail: detail}
}

// failCheck keeps a failure on one line. A joined error spans lines, and the check line is the output contract.
func failCheck(name string, err error) Check {
	return Check{Name: name, Status: Fail, Detail: strings.ReplaceAll(err.Error(), "\n", "; ")}
}

func skipCheck(name, reason string) Check {
	return Check{Name: name, Status: Skip, Detail: reason}
}

// Summary counts the checks and reports whether any failed.
func Summary(w io.Writer, checks []Check) (failed bool) {
	var ok, fail, skip int
	for _, c := range checks {
		switch c.Status {
		case OK:
			ok++
		case Fail:
			fail++
		case Skip:
			skip++
		}
	}
	_, _ = fmt.Fprintf(w, "summary: %d ok, %d fail, %d skip\n", ok, fail, skip)
	return fail > 0
}

// CompletionProven reports whether a live run finished its full path: some run's completion step passed.
// A run with skipped steps can pass every other step and still prove nothing about the merge.
func CompletionProven(checks []Check) bool {
	for _, c := range checks {
		if c.Status == OK && strings.HasSuffix(c.Name, ".completion") {
			return true
		}
	}
	return false
}
