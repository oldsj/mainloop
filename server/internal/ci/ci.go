// Package ci reads a project's CI results for its agents: which checks ran on a commit, whether
// they passed, and why the failed ones failed. A [Provider] hides the forge (GitHub today; GitLab or
// Bitbucket later) behind one normalized shape, and the judgment of a check's result lives here so
// every reader (the check tools and the merge evidence) agrees on what passed, failed or is pending.
package ci

import (
	"context"
	"errors"
	"slices"
)

// Results of one check.
const (
	Passed  = "passed"
	Failed  = "failed"
	Pending = "pending"
)

// pendingStates are the in-flight states of a check run, suite or commit status.
var pendingStates = []string{"queued", "in_progress", "pending", "waiting", "requested"}

// passingConclusions are the completed conclusions that count as passed: a plain success, and
// skipped, which GitHub itself does not treat as blocking a merge. neutral is deliberately
// excluded: it is not an assertion that the check ran and passed.
var passingConclusions = []string{"success", "skipped"}

// RunResult judges a check run or suite. A completed success or skipped passes; every other
// completed conclusion (neutral included) and every unfamiliar state fails, so a result the
// provider reports in a way Mainloop does not know is never green.
func RunResult(status, conclusion string) string {
	switch {
	case status == "completed" && slices.Contains(passingConclusions, conclusion):
		return Passed
	case status == "completed":
		return Failed
	case slices.Contains(pendingStates, status):
		return Pending
	}
	return Failed
}

// StatusResult judges a commit status: success passes, pending is pending, anything else fails.
func StatusResult(state string) string {
	switch state {
	case "success":
		return Passed
	case "pending":
		return Pending
	}
	return Failed
}

// Kinds of check.
const (
	KindCheckRun     = "check_run"
	KindCommitStatus = "commit_status"
	// KindCheckSuite is a suite still running with no pending run of its own; it is always pending.
	KindCheckSuite = "check_suite"
)

// Repo names a repository at its provider.
type Repo struct{ Owner, Name string }

// Check is one check on a commit, the latest of its name: earlier attempts of the same check are
// not listed. ID is the provider's own id, for [Provider.Failure]. Status is the provider's state
// text (a conclusion once completed). Summary is the check's own short output and Text its longer
// output, when it has them; listings show only Summary.
type Check struct {
	ID          int64
	Kind        string
	Name        string
	App         string
	Status      string
	Result      string
	URL         string
	StartedAt   string
	CompletedAt string
	Summary     string
	Text        string
}

// Annotation is a file location a check reported, with its message.
type Annotation struct {
	Path      string
	StartLine int
	EndLine   int
	Level     string
	Title     string
	Message   string
}

// Failure is why one check failed, as far as its provider tells: its output and annotations, and
// the tail of its log where the provider can read one. LogTail is empty when it cannot (on GitHub,
// until the App holds actions:read).
type Failure struct {
	Name        string
	URL         string
	Summary     string
	Text        string
	Annotations []Annotation
	LogTail     string
}

// ErrNotFound means the provider has no such pull request or commit.
var ErrNotFound = errors.New("ci: not found")

// MergedHead is the head commit a merged pull request was merged from.
type MergedHead struct {
	PR  int
	SHA string
}

// Provider reads one forge's CI. Implementations make no writes.
type Provider interface {
	// HeadSHA resolves a pull request to its current head commit.
	HeadSHA(ctx context.Context, repo Repo, pr int) (string, error)
	// BranchHead resolves a branch to the commit it points at.
	BranchHead(ctx context.Context, repo Repo, branch string) (string, error)
	// MergedHeads lists the head commits of the pull requests most recently merged into branch,
	// newest first, with their numbers.
	MergedHeads(ctx context.Context, repo Repo, branch string, limit int) ([]MergedHead, error)
	// Checks lists the latest check of each name on a commit.
	Checks(ctx context.Context, repo Repo, sha string) ([]Check, error)
	// Failure reads why a failed check failed.
	Failure(ctx context.Context, repo Repo, c Check) (Failure, error)
}

// State is the verdict over all the checks of a commit.
type State string

// States. Failed wins over Pending: a commit with one failed check is failed even while others
// still run, so the reader can start on the failure.
const (
	Green    State = "green"
	Red      State = "failed"
	Running  State = "pending"
	NoChecks State = "no_checks"
)

// Counts summarises the checks.
type Counts struct{ Passed, Failed, Pending int }

// Judge gives the verdict over checks. No checks at all is [NoChecks], never green.
func Judge(checks []Check) (State, Counts) {
	var c Counts
	for _, ch := range checks {
		switch ch.Result {
		case Passed:
			c.Passed++
		case Pending:
			c.Pending++
		default:
			c.Failed++
		}
	}
	switch {
	case len(checks) == 0:
		return NoChecks, c
	case c.Failed > 0:
		return Red, c
	case c.Pending > 0:
		return Running, c
	}
	return Green, c
}
