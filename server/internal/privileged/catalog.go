package privileged

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// Catalog types.
const (
	TypePRMerge = "github.pr_merge"
	TypePRClose = "github.pr_close"
)

// CatalogVersion is bumped when any entry's meaning changes; it is part of every digest.
const CatalogVersion = 1

// Bounds of the GitHub types.
const (
	MaxClosePRs     = 20
	MaxCommentLen   = 2000
	maxFailingNames = 5
)

// GitHub is what the GitHub types read and write, through Mainloop's App.
type GitHub interface {
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequestInfo, error)
	CheckRuns(ctx context.Context, repo github.Repo, sha string) ([]github.CheckRun, error)
	CommitStatuses(ctx context.Context, repo github.Repo, sha string) ([]github.CommitStatus, error)
	MergePullRequest(ctx context.Context, repo github.Repo, number int, sha string) (*github.MergeResult, error)
	ClosePullRequest(ctx context.Context, repo github.Repo, number int) error
	CommentIssue(ctx context.Context, repo github.Repo, number int, body string) (*github.IssueWrite, error)
}

// entry is one catalog type.
type entry struct {
	version int32
	expiry  time.Duration
	// expiryFor, if set, gives the expiry from the canonical arguments instead: a sync that may
	// prune is a delete.
	expiryFor func(args any) time.Duration
	// executor types hold a credential serve-api must not have: serve-executor preflights and
	// runs them. The others run in serve-api.
	executor bool
	// kube types also need the executor's Kubernetes access (executor.kubernetes in the chart).
	kube bool
	// omni types need the executor's Omni access (executor.omni in the chart).
	omni bool
	// ssh types need the executor's ssh access (executor.ssh in the chart).
	ssh bool
	// how is the card's line on what Approve does and through which credential.
	how string
	// normalize validates raw arguments and returns their canonical value.
	normalize func(raw json.RawMessage) (any, error)
	// facts is the zero value of the type's frozen facts, which preflight returns and execute
	// decodes; the executor refuses a row whose facts don't round-trip through it (exactRow).
	facts any
	// preflight reads the facts the card shows and the executor's preconditions through the
	// service's clients. A *refusal means the action cannot go to the owner as asked.
	preflight func(ctx context.Context, s *Service, args json.RawMessage, now time.Time) (any, error)
}

const githubHow = "Approve runs exactly this once, through Mainloop's GitHub App; nothing bypasses a branch rule."

var catalog = map[string]entry{
	TypePRMerge: {version: 1, expiry: UndecidedExpiry, how: githubHow, normalize: normalizeMerge, facts: MergeFacts{}, preflight: preflightMerge},
	TypePRClose: {version: 1, expiry: UndecidedExpiry, how: githubHow, normalize: normalizeClose, facts: CloseFacts{}, preflight: preflightClose},
	TypeItemToSecret: {
		version: 1, expiry: UndecidedExpiry, executor: true,
		how: "Approve copies exactly this field once, through Mainloop's executor; the value never reaches an agent, " +
			"the API process or this card.",
		normalize: normalizeItemToSecret, facts: ItemToSecretFacts{}, preflight: preflightItemToSecret,
	},
	TypeRolloutRestart: {
		version: 1, expiry: UndecidedExpiry, executor: true, kube: true, how: kubeHow,
		normalize: normalizeRestart, facts: WorkloadFacts{}, preflight: preflightRestart,
	},
	TypeScale: {
		version: 1, expiry: UndecidedExpiry, executor: true, kube: true, how: kubeHow,
		normalize: normalizeScale, facts: WorkloadFacts{}, preflight: preflightScale,
	},
	TypeDelete: {
		version: 1, expiry: DeleteExpiry, executor: true, kube: true, how: kubeHow,
		normalize: normalizeDelete, facts: DeleteFacts{}, preflight: preflightDelete,
	},
	TypePatch: {
		version: 1, expiry: UndecidedExpiry, executor: true, kube: true, how: kubeHow,
		normalize: normalizePatch, facts: ChangeFacts{}, preflight: preflightPatch,
	},
	TypeApply: {
		version: 1, expiry: UndecidedExpiry, executor: true, kube: true, how: kubeHow,
		normalize: normalizeApply, facts: ChangeFacts{}, preflight: preflightApply,
	},
	TypeArgoSync: {
		version: 1, expiry: UndecidedExpiry, expiryFor: argoExpiry, executor: true, kube: true, how: kubeHow,
		normalize: normalizeArgoSync, facts: ArgoFacts{}, preflight: preflightArgoSync,
	},
	TypeKubectl: {
		version: 1, expiry: UndecidedExpiry, expiryFor: kubectlExpiry, executor: true, kube: true,
		how: "Approve runs exactly this command once, as cluster-admin, through Mainloop's executor. " +
			"kubectl never holds the credential: the executor's proxy does, and refuses its own namespace, Secrets, impersonation and admission policies.",
		normalize: normalizeKubectl, facts: KubectlFacts{}, preflight: preflightKubectl,
	},
	TypeOmnictl: {
		version: 1, expiry: UndecidedExpiry, expiryFor: sideroExpiry(ToolOmnictl), executor: true, omni: true, how: sideroHow,
		normalize: normalizeSidero(ToolOmnictl), facts: SideroFacts{}, preflight: preflightSidero(ToolOmnictl),
	},
	TypeTalosctl: {
		version: 1, expiry: UndecidedExpiry, expiryFor: sideroExpiry(ToolTalosctl), executor: true, omni: true, how: sideroHow,
		normalize: normalizeSidero(ToolTalosctl), facts: SideroFacts{}, preflight: preflightSidero(ToolTalosctl),
	},
	TypeSSH: {
		version: 1, expiry: UndecidedExpiry, expiryFor: sshExpiry, executor: true, ssh: true,
		how: "Approve runs exactly this command once on the host, over ssh, as its admin user with the executor's key for that host. " +
			"Nothing previews it, and nothing on the executor's side limits what the remote shell does with it.",
		normalize: normalizeSSH, facts: SSHFacts{}, preflight: preflightSSH,
	},
}

const sideroHow = "Approve runs exactly this command once, through Mainloop's executor, as its Omni service account (role Admin). " +
	"The tool never holds that key: the executor's proxy signs each request, and refuses secrets, credential minting and other clusters."

// TypesFor lists the types one process runs, sorted: the executor's, or serve-api's.
func TypesFor(executor bool) []string {
	var out []string
	for t, e := range catalog {
		if e.executor == executor {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// Types lists the catalog's types, sorted.
func Types() []string {
	out := make([]string, 0, len(catalog))
	for t := range catalog {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// refusal is a preflight finding that ends the request: the target is not in a state the action
// can apply to. Its message is shown to the agent.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, a ...any) error { return &refusal{msg: fmt.Sprintf(format, a...)} }

// MergeArgs are github.pr_merge's arguments. The merge is a squash at exactly ExpectedHeadSHA.
type MergeArgs struct {
	Repo            string `json:"repo"`
	PR              int    `json:"pr"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
}

// CloseArgs are github.pr_close's arguments.
type CloseArgs struct {
	Repo    string `json:"repo"`
	PRs     []int  `json:"prs"`
	Comment string `json:"comment,omitempty"`
}

// PRFacts are what preflight read about one pull request.
type PRFacts struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Author         string `json:"author,omitempty"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	HeadSHA        string `json:"head_sha"`
	HeadRef        string `json:"head_ref"`
	BaseRef        string `json:"base_ref"`
	HTMLURL        string `json:"html_url"`
	Mergeable      *bool  `json:"mergeable,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
	ChangedFiles   int    `json:"changed_files"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
}

// Checks counts the checks and statuses on a commit.
type Checks struct {
	Read    bool     `json:"read"`
	Passed  int      `json:"passed"`
	Failed  int      `json:"failed"`
	Pending int      `json:"pending"`
	Failing []string `json:"failing,omitempty"`
}

// MergeFacts are github.pr_merge's frozen facts.
type MergeFacts struct {
	Repo      string    `json:"repo"`
	PR        PRFacts   `json:"pr"`
	Checks    Checks    `json:"checks"`
	CheckedAt time.Time `json:"checked_at"`
}

// CloseFacts are github.pr_close's frozen facts.
type CloseFacts struct {
	Repo      string    `json:"repo"`
	PRs       []PRFacts `json:"prs"`
	CheckedAt time.Time `json:"checked_at"`
}

func decodeStrict(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: args: %s", ErrInvalid, err.Error())
	}
	return nil
}

func parseRepo(s string) (github.Repo, error) {
	repo, err := github.ParseRepo(strings.TrimSpace(s))
	if err != nil {
		return github.Repo{}, fmt.Errorf("%w: repo must be owner/name", ErrInvalid)
	}
	return repo, nil
}

func normalizeMerge(raw json.RawMessage) (any, error) {
	var a MergeArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	repo, err := parseRepo(a.Repo)
	if err != nil {
		return nil, err
	}
	a.Repo = repo.FullName()
	if a.PR <= 0 {
		return nil, fmt.Errorf("%w: pr must be a pull request number", ErrInvalid)
	}
	a.ExpectedHeadSHA = strings.ToLower(strings.TrimSpace(a.ExpectedHeadSHA))
	if !isSHA(a.ExpectedHeadSHA) {
		return nil, fmt.Errorf("%w: expected_head_sha must be the 40-character head commit", ErrInvalid)
	}
	return a, nil
}

func normalizeClose(raw json.RawMessage) (any, error) {
	var a CloseArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	repo, err := parseRepo(a.Repo)
	if err != nil {
		return nil, err
	}
	a.Repo = repo.FullName()
	if len(a.PRs) == 0 || len(a.PRs) > MaxClosePRs {
		return nil, fmt.Errorf("%w: prs must list 1 to %d pull request numbers", ErrInvalid, MaxClosePRs)
	}
	for _, n := range a.PRs {
		if n <= 0 {
			return nil, fmt.Errorf("%w: prs must be pull request numbers", ErrInvalid)
		}
	}
	slices.Sort(a.PRs)
	a.PRs = slices.Compact(a.PRs)
	a.Comment = strings.TrimSpace(a.Comment)
	if utf8.RuneCountInString(a.Comment) > MaxCommentLen {
		return nil, fmt.Errorf("%w: comment is longer than %d characters", ErrInvalid, MaxCommentLen)
	}
	if a.Comment != redact.Text(a.Comment) {
		return nil, fmt.Errorf("%w: comment looks like it contains a secret; it would be shown on the card and posted", ErrInvalid)
	}
	return a, nil
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func prFacts(p *github.PullRequestInfo) PRFacts {
	return PRFacts{
		Number: p.Number, Title: p.Title, Author: p.Author, State: p.State, Draft: p.Draft,
		HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, HTMLURL: p.HTMLURL,
		Mergeable: p.Mergeable, MergeableState: p.MergeableState, ChangedFiles: p.ChangedFiles,
		Additions: p.Additions, Deletions: p.Deletions,
	}
}

// readPR reads one pull request; a missing one is a refusal.
func readPR(ctx context.Context, gh GitHub, repo github.Repo, n int) (*github.PullRequestInfo, error) {
	p, err := gh.PullRequest(ctx, repo, n)
	switch {
	case errors.Is(err, github.ErrNotFound):
		return nil, refuse("%s#%d was not found, or Mainloop's GitHub App cannot see it", repo.FullName(), n)
	case errors.Is(err, github.ErrNotInstalled):
		return nil, refuse("Mainloop's GitHub App is not installed on %s", repo.FullName())
	case err != nil:
		return nil, err
	}
	return p, nil
}

func preflightMerge(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	gh := s.GitHub
	var a MergeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	repo, err := github.ParseRepo(a.Repo)
	if err != nil {
		return nil, err
	}
	p, err := readPR(ctx, gh, repo, a.PR)
	if err != nil {
		return nil, err
	}
	switch {
	case p.Merged:
		return nil, refuse("%s#%d is already merged", a.Repo, a.PR)
	case p.State != "open":
		return nil, refuse("%s#%d is %s", a.Repo, a.PR, p.State)
	case p.Draft:
		return nil, refuse("%s#%d is a draft; mark it ready first", a.Repo, a.PR)
	case p.Head.SHA != a.ExpectedHeadSHA:
		return nil, refuse("%s#%d's head is %s, not the expected %s; request again with the current head", a.Repo, a.PR, p.Head.SHA, a.ExpectedHeadSHA)
	}
	return MergeFacts{Repo: a.Repo, PR: prFacts(p), Checks: readChecks(ctx, gh, repo, p.Head.SHA), CheckedAt: now}, nil
}

// readChecks summarises the head's check runs and commit statuses. A read that fails is recorded
// as unread rather than failing the request: the card says so, and GitHub's own rules still
// refuse a merge they require checks for.
func readChecks(ctx context.Context, gh GitHub, repo github.Repo, sha string) Checks {
	runs, err := gh.CheckRuns(ctx, repo, sha)
	if err != nil {
		return Checks{}
	}
	statuses, err := gh.CommitStatuses(ctx, repo, sha)
	if err != nil {
		return Checks{}
	}
	c := Checks{Read: true}
	fail := func(name string) {
		c.Failed++
		if len(c.Failing) < maxFailingNames {
			c.Failing = append(c.Failing, name)
		}
	}
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			c.Pending++
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
			c.Passed++
		default:
			fail(r.Name)
		}
	}
	// Statuses come newest first; only the newest of each context counts.
	seen := map[string]bool{}
	for _, st := range statuses {
		if seen[st.Context] {
			continue
		}
		seen[st.Context] = true
		switch st.State {
		case "success":
			c.Passed++
		case "pending":
			c.Pending++
		default:
			fail(st.Context)
		}
	}
	return c
}

func preflightClose(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	gh := s.GitHub
	var a CloseArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	repo, err := github.ParseRepo(a.Repo)
	if err != nil {
		return nil, err
	}
	f := CloseFacts{Repo: a.Repo, CheckedAt: now}
	for _, n := range a.PRs {
		p, err := readPR(ctx, gh, repo, n)
		if err != nil {
			return nil, err
		}
		if p.Merged || p.State != "open" {
			return nil, refuse("%s#%d is not open; leave it out", a.Repo, n)
		}
		f.PRs = append(f.PRs, prFacts(p))
	}
	return f, nil
}

// canonical is v's JSON with sorted keys, no HTML escaping and no insignificant space: the form
// the digest is computed over. Numbers keep their text.
func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonicalRaw(raw)
}

func canonicalRaw(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Digest is the SHA-256 of the action's canonical record (CanonicalRecord).
func Digest(row dbq.PrivilegedAction, facts []byte) (string, error) {
	b, err := CanonicalRecord(row, facts)
	if err != nil {
		return "", err
	}
	return hashHex(b), nil
}

// CanonicalRecord is the canonical JSON of everything the owner's approval binds: the type and
// its version, the arguments, the frozen facts, the requester, the action id, the expiry and the
// catalog version. The agent's reason is not part of it. The digest is its SHA-256; a phone hashes
// these bytes itself and shows what it parsed from them, so what it signs is what it showed.
func CanonicalRecord(row dbq.PrivilegedAction, facts []byte) ([]byte, error) {
	args, err := canonicalRaw(row.Args)
	if err != nil {
		return nil, err
	}
	f, err := canonicalRaw(facts)
	if err != nil {
		return nil, err
	}
	in := map[string]any{
		"action_id": row.ID, "type": row.Type, "version": row.Version, "catalog_version": CatalogVersion,
		"args": json.RawMessage(args), "frozen_facts": json.RawMessage(f),
		"requester":  map[string]any{"role": row.RequesterRole, "session_id": row.RequesterSessionID, "task_id": row.RequesterTaskID},
		"expires_at": row.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	return canonical(in)
}

// safeError is an error's text with secrets removed and its length bounded, for results, events
// and messages.
func safeError(err error) string {
	return redact.Line(err.Error(), 500)
}
