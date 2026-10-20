package pushgate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gitref"
	ka "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// deployBranch is the branch Argo tracks for production. CI alone writes it, so no policy, task
// or allowlist can grant it.
const deployBranch = "release"

// Policy is server-owned. Protected and all previously observed default branches override
// AllowedPatterns. An absent allowlist permits feature branches; an absent default denies all.
type Policy struct {
	DefaultBranch     string   `json:"default_branch"`
	PreviousDefaults  []string `json:"previous_defaults"`
	ProtectedPatterns []string `json:"protected_patterns"`
	AllowedPatterns   []string `json:"allowed_patterns"`
}

// Allows lets creation callers reserve a reference only for a branch the stored policy permits.
// Enrollment and each publication still recheck the policy and fresh repository metadata.
func (p Policy) Allows(branch string) bool { return p.permits(branch) == nil }

// Reason names why Allows would refuse branch, "" when it would not. Callers outside this package
// use it to report the same policy-level denial the gate itself would hit, without reaching the
// gate's finer-grained internal denials (those cover protocol and runtime checks this reports as
// a plain refusal).
func (p Policy) Reason(branch string) string {
	if err := p.permits(branch); err != nil {
		return err.Error()
	}
	return ""
}

func (p Policy) permits(branch string) error {
	if !gitref.Valid(p.DefaultBranch) {
		return denial("metadata_unavailable")
	}
	if !gitref.ValidCheckoutRef(branch) {
		return denial("invalid_branch")
	}
	if branch == p.DefaultBranch {
		return denial("default_branch")
	}
	if branch == deployBranch {
		return denial("protected_branch")
	}
	for _, b := range p.PreviousDefaults {
		if branch == b {
			return denial("protected_branch")
		}
	}
	for _, pattern := range p.ProtectedPatterns {
		ok, err := matchBranch(pattern, branch)
		if err != nil || ok {
			return denial("protected_branch")
		}
	}
	if len(p.AllowedPatterns) > 0 {
		for _, pattern := range p.AllowedPatterns {
			ok, err := matchBranch(pattern, branch)
			if err != nil {
				return denial("policy_mismatch")
			}
			if ok {
				return nil
			}
		}
		return denial("branch_mismatch")
	}
	return nil
}

// Python's fnmatchcase treats '/' as an ordinary branch character. Preserve that protection:
// e.g. '*' protects every nested branch, not just branches without a slash.
func matchBranch(pattern, branch string) (bool, error) {
	if strings.ContainsAny(pattern, "\\\x00") {
		return false, denial("policy_mismatch")
	}
	var expression strings.Builder
	expression.WriteString("(?s)^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			expression.WriteString(".*")
		case '?':
			expression.WriteByte('.')
		case '[':
			end := i + 1
			negate := false
			if end < len(pattern) && pattern[end] == '!' {
				negate = true
				end++
			}
			start := end
			if end < len(pattern) && pattern[end] == ']' {
				end++
			}
			for end < len(pattern) && pattern[end] != ']' {
				end++
			}
			if end == len(pattern) || end == start {
				return false, denial("policy_mismatch")
			}
			expression.WriteByte('[')
			if negate {
				expression.WriteByte('^')
			}
			for _, r := range pattern[start:end] {
				if r == '^' || r == '[' || r == ']' {
					expression.WriteByte('\\')
				}
				expression.WriteRune(r)
			}
			expression.WriteByte(']')
			i = end
		default:
			// Quote a full UTF-8 span as well as ASCII metacharacters.
			end := i + 1
			for end < len(pattern) && pattern[end]&0xc0 == 0x80 {
				end++
			}
			expression.WriteString(regexp.QuoteMeta(pattern[i:end]))
			i = end - 1
		}
	}
	expression.WriteByte('$')
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return false, denial("policy_mismatch")
	}
	return compiled.MatchString(branch), nil
}

func (p Policy) update(branch string, u Update) error {
	if err := p.permits(branch); err != nil {
		return err
	}
	if !strings.HasPrefix(u.Ref, "refs/heads/") {
		return denial("ref_namespace")
	}
	if u.Ref != "refs/heads/"+branch {
		return denial("branch_mismatch")
	}
	if u.New == zeroOID {
		return denial("deletion")
	}
	return nil
}

type Native interface {
	GetSession(context.Context, string) (*ka.Session, error)
}

// Service is the push gate's authority: it admits and publishes pushes, resolves them, and enrolls
// a workspace's push credential. A process builds exactly one, with [NewService].
type Service struct {
	Pool        *pgxpool.Pool
	Native      Native
	Upstream    Upstream
	Credentials *agentauth.Credentials
}

// ErrEnrollUnavailable means this process cannot enroll: it has no credential service or no upstream.
var ErrEnrollUnavailable = errors.New("pushgate: this process cannot enroll a push credential")

// NewService builds the gate over the database, the native runtime and the upstream.
//
// A nil upstream is a gate that is switched off: the process still advances push policy, and
// nothing else works. serve-git passes no credentials because it never enrolls (the API process owns
// the credential Secrets and runs Enroll); [Service.Enroll] then returns [ErrEnrollUnavailable]
// instead of reaching a nil credential service.
func NewService(pool *pgxpool.Pool, native Native, upstream Upstream, creds *agentauth.Credentials) *Service {
	return &Service{Pool: pool, Native: native, Upstream: upstream, Credentials: creds}
}

type capability struct {
	WorkspaceID  string
	CredentialID string
}
type authority struct {
	W             dbq.GetPushWorkspaceRow
	G             dbq.GitPushGrant
	P             Policy
	PolicyVersion int64
}

func textPointer(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func (s *Service) authenticate(ctx context.Context, header string) (capability, error) {
	token, err := agentauth.BearerToken(header)
	if err != nil {
		return capability{}, denial("authentication_required")
	}
	row, err := dbq.New(s.Pool).GetPushCapability(ctx, agentauth.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return capability{}, denial("authentication_required")
	}
	if err != nil {
		return capability{}, err
	}
	return capability{row.WorkspaceID, row.CredentialID}, nil
}
func load(ctx context.Context, q *dbq.Queries, c capability) (authority, error) {
	var a authority
	w, err := q.GetPushWorkspace(ctx, c.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, denial("writer_claim_lost")
	}
	if err != nil {
		return a, err
	}
	g, err := q.GetPushGrant(ctx, c.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, denial("grant_revoked")
	}
	if err != nil {
		return a, err
	}
	if g.State != dbstate.GitGrantPublished || textPointer(g.CredentialID) != c.CredentialID {
		return a, denial("grant_revoked")
	}
	cred, err := q.GetPushCredential(ctx, c.CredentialID)
	if err != nil {
		return a, err
	}
	if cred.State != dbstate.CredentialPublished || cred.Kind != dbstate.CredentialGitPush || cred.BindingID != w.ID {
		return a, denial("credential_revoked")
	}
	if g.WriterGeneration != w.Generation || g.OwnerID != w.UserID || textPointer(g.AttemptID) != textPointer(w.AttemptID) {
		return a, denial("stale_writer_generation")
	}
	if g.Branch != w.Branch || g.NativeSessionID != w.NativeSessionID {
		return a, denial("binding_scope")
	}
	p, err := q.GetPushPolicy(ctx, w.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, denial("policy_mismatch")
	}
	if err != nil {
		return a, err
	}
	if p.Version < g.PolicyVersion || json.Unmarshal(p.Policy, &a.P) != nil {
		return a, denial("policy_mismatch")
	}
	if a.P.DefaultBranch != w.DefaultBranch {
		return a, denial("metadata_unavailable")
	}
	repo, err := github.ParseRepo(w.RepoURL)
	if err != nil || !strings.EqualFold(repo.FullName(), w.Repository) {
		return a, denial("repository_mismatch")
	}
	if err = a.P.permits(w.Branch); err != nil {
		return a, err
	}
	a.W = w
	a.G = g
	a.PolicyVersion = p.Version
	return a, nil
}

// PinnedEnvironment decodes a workspace's recorded development environment. ok is false when the
// workspace was created without one (its project had no selection) or the record is incomplete: such
// a workspace can never be attested, so it can never hold push authority.
func PinnedEnvironment(raw json.RawMessage) (e api.WorkspaceEnvironment, ok bool) {
	if json.Unmarshal(raw, &e) != nil || e.Image == "" || e.Platform == "" || e.PolicyIdentity == "" {
		return api.WorkspaceEnvironment{}, false
	}
	return e, true
}

func (s *Service) observe(ctx context.Context, w dbq.GetPushWorkspaceRow) (string, error) {
	if s.Native == nil {
		return "", denial("runtime_unattested")
	}
	n, err := s.Native.GetSession(ctx, w.NativeSessionID)
	if err != nil {
		return "", denial("runtime_unattested")
	}
	ra := n.GetRuntimeAssociation()
	nw := n.GetWorkspace()
	env := n.GetDevelopmentEnvironment()
	expected, ok := PinnedEnvironment(w.Environment)
	if !ok {
		return "", denial("runtime_unattested")
	}
	if n.GetId() != w.NativeSessionID || n.GetContextId() != w.NativeSessionID || n.GetState() != ka.RuntimeState_RUNTIME_STATE_READY || n.GetOperation() != ka.RuntimeOperation_RUNTIME_OPERATION_NONE || n.GetPreparedRevision() == "" || n.GetRuntimeComposition() == nil ||
		ra.GetGenerationId() == "" || ra.GetAtespace() == "" || ra.GetActorName() == "" || ra.GetActorUid() == "" || ra.GetPhase() != "active" || !ra.GetCurrentActive() ||
		n.GetAgent().GetNamespace() != w.AgentNamespace || n.GetAgent().GetName() != w.AgentName || nw.GetRepo() != w.RepoURL || nw.GetRef() != w.Ref || nw.GetBranch() != w.Branch || nw.GetDepth() != w.Depth ||
		env.GetImage() != expected.Image || env.GetPlatform() != expected.Platform || env.GetPolicyIdentity() != expected.PolicyIdentity {
		return "", denial("runtime_unattested")
	}
	frozen := &ka.Session{Id: n.GetId(), ContextId: n.GetContextId(), Agent: n.GetAgent(), Workspace: nw, DevelopmentEnvironment: env, RuntimeComposition: n.GetRuntimeComposition(), PreparedRevision: n.GetPreparedRevision(), RuntimeAssociation: ra}
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(frozen)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Service) check(ctx context.Context, c capability, skip string) (authority, error) {
	a, err := load(ctx, dbq.New(s.Pool), c)
	if err != nil {
		return a, err
	}
	digest, err := s.observe(ctx, a.W)
	if err != nil {
		return a, err
	}
	var runtime struct {
		Digest string `json:"digest"`
	}
	if json.Unmarshal(a.G.Runtime, &runtime) != nil || runtime.Digest != digest {
		return a, denial("runtime_changed")
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.LockPushWriter(ctx, dbq.LockPushWriterParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch}); err != nil {
			return err
		}
		cur, err := load(ctx, q, c)
		if err != nil {
			return err
		}
		if !sameAuthority(a, cur) {
			return denial("authority_changed")
		}
		if skip != "" {
			if err = requireAdmission(ctx, q, skip); err != nil {
				return err
			}
		}
		busy, err := q.PushAdmissionHeld(ctx, dbq.PushAdmissionHeldParams{Repository: a.W.Repository, Branch: a.W.Branch, ID: skip})
		if err != nil {
			return err
		}
		if busy {
			return denial("admission_busy")
		}
		held, err := q.PushBranchHeld(ctx, dbq.PushBranchHeldParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch, ID: skip})
		if err != nil {
			return err
		}
		if held {
			return denial("publication_unresolved")
		}
		return nil
	})
	return a, err
}
func sameAuthority(a, b authority) bool {
	// Activity timestamps are deliberately excluded: a turn may update them without changing scope.
	return a.W.ID == b.W.ID && a.W.UserID == b.W.UserID && a.W.ProjectID == b.W.ProjectID && a.W.Repository == b.W.Repository && a.W.Branch == b.W.Branch && a.W.Ref == b.W.Ref && a.W.Depth == b.W.Depth && a.W.AgentNamespace == b.W.AgentNamespace && a.W.AgentName == b.W.AgentName &&
		a.W.NativeSessionID == b.W.NativeSessionID && a.W.Generation == b.W.Generation && reflect.DeepEqual(a.W.Environment, b.W.Environment) && reflect.DeepEqual(a.G, b.G) && reflect.DeepEqual(a.P, b.P) && a.PolicyVersion == b.PolicyVersion
}

// Enroll freezes the held writer generation and confirmed native runtime, then publishes a push
// Secret through the existing credential service. Nothing on the authentication path calls it.
// Native creation callers must reserve Reference before creation and call Enroll after readiness.
func (s *Service) Enroll(ctx context.Context, id string) (agentauth.Issued, error) {
	if s.Credentials == nil || s.Upstream == nil {
		return agentauth.Issued{}, ErrEnrollUnavailable
	}
	q := dbq.New(s.Pool)
	w, err := q.GetPushWorkspace(ctx, id)
	if err != nil {
		return agentauth.Issued{}, err
	}
	repo, err := github.ParseRepo(w.Repository)
	if err != nil {
		return agentauth.Issued{}, err
	}
	def, err := s.Upstream.DefaultBranch(ctx, repo)
	if err != nil || !gitref.Valid(def) || def != w.DefaultBranch {
		return agentauth.Issued{}, denial("metadata_unavailable")
	}
	initial, _ := json.Marshal(Policy{DefaultBranch: def})
	if err = q.InsertPushPolicy(ctx, dbq.InsertPushPolicyParams{ProjectID: w.ProjectID, Policy: initial}); err != nil {
		return agentauth.Issued{}, err
	}
	p, err := q.GetPushPolicy(ctx, w.ProjectID)
	if err != nil {
		return agentauth.Issued{}, err
	}
	var policy Policy
	if json.Unmarshal(p.Policy, &policy) != nil {
		return agentauth.Issued{}, denial("policy_mismatch")
	}
	if err = policy.permits(w.Branch); err != nil {
		return agentauth.Issued{}, err
	}
	digest, err := s.observe(ctx, w)
	if err != nil {
		return agentauth.Issued{}, err
	}
	runtime, _ := json.Marshal(map[string]string{"digest": digest})
	var grant dbq.GitPushGrant
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		if _, err := tq.LockPushWriter(ctx, dbq.LockPushWriterParams{OwnerID: w.UserID, Repository: w.Repository, Branch: w.Branch}); err != nil {
			return err
		}
		cur, err := tq.GetPushWorkspace(ctx, id)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(w, cur) {
			return denial("authority_changed")
		}
		held, err := tq.PushBranchHeld(ctx, dbq.PushBranchHeldParams{OwnerID: w.UserID, Repository: w.Repository, Branch: w.Branch})
		if err != nil {
			return err
		}
		if held {
			return denial("publication_unresolved")
		}
		grant, err = tq.PlanPushGrant(ctx, dbq.PlanPushGrantParams{WorkspaceID: id, Branch: w.Branch, PolicyVersion: p.Version, OwnerID: w.UserID, WriterGeneration: w.Generation, AttemptID: w.AttemptID, NativeSessionID: w.NativeSessionID, Runtime: runtime})
		return err
	})
	if err != nil {
		return agentauth.Issued{}, err
	}
	var saved struct {
		Digest string `json:"digest"`
	}
	if json.Unmarshal(grant.Runtime, &saved) != nil || saved.Digest != digest || grant.WriterGeneration != w.Generation || grant.PolicyVersion > p.Version || grant.OwnerID != w.UserID || grant.Branch != w.Branch || grant.NativeSessionID != w.NativeSessionID || textPointer(grant.AttemptID) != textPointer(w.AttemptID) || grant.State == dbstate.GitGrantRevoked {
		return agentauth.Issued{}, denial("authority_changed")
	}
	issued, err := s.Credentials.Issue(ctx, id, dbstate.CredentialGitPush)
	if err != nil {
		return issued, err
	}
	if grant.State == dbstate.GitGrantPublished {
		if textPointer(grant.CredentialID) != issued.CredentialID {
			return issued, denial("credential_revoked")
		}
		return issued, nil
	}
	_, err = q.PublishPushGrant(ctx, dbq.PublishPushGrantParams{WorkspaceID: id, CredentialID: &issued.CredentialID})
	return issued, err
}

// Reference supplies the fixed Secret reference before native Session creation. This publishes
// no bearer and grants no authority.
func Reference(c *agentauth.Credentials, origin, id string) *ka.SessionCredential {
	return &ka.SessionCredential{Origin: origin, Header: "authorization", SecretRef: &ka.SecretKeyReference{Name: c.SecretName(dbstate.CredentialGitPush, id), Key: "authorization"}}
}

// SetPolicy is an internal compare-and-swap for trusted metadata/policy callers. It retains
// every observed default; callers cannot release a former default through a policy update.
// It is not an owner route (owner policy writes remain gated by the existing API).
func (s *Service) SetPolicy(ctx context.Context, project string, version int64, next Policy) error {
	if !gitref.Valid(next.DefaultBranch) || len(next.ProtectedPatterns) > 64 || len(next.AllowedPatterns) > 64 {
		return denial("policy_mismatch")
	}
	for _, pattern := range append(append([]string{}, next.ProtectedPatterns...), next.AllowedPatterns...) {
		if len(pattern) > gitref.MaxLen {
			return denial("policy_mismatch")
		}
		if _, err := matchBranch(pattern, ""); err != nil {
			return denial("policy_mismatch")
		}
	}
	q := dbq.New(s.Pool)
	old, err := q.GetPushPolicy(ctx, project)
	if err != nil {
		return err
	}
	var policy Policy
	if old.Version != version || json.Unmarshal(old.Policy, &policy) != nil || !gitref.Valid(policy.DefaultBranch) {
		return denial("policy_mismatch")
	}
	seen := map[string]bool{next.DefaultBranch: true}
	next.PreviousDefaults = nil
	for _, branch := range append(policy.PreviousDefaults, policy.DefaultBranch) {
		if !gitref.Valid(branch) {
			return denial("policy_mismatch")
		}
		if !seen[branch] {
			next.PreviousDefaults = append(next.PreviousDefaults, branch)
			seen[branch] = true
		}
	}
	b, _ := json.Marshal(next)
	n, err := q.UpdatePushPolicy(ctx, dbq.UpdatePushPolicyParams{ProjectID: project, Version: version, Policy: b})
	if err != nil {
		return err
	}
	if n != 1 {
		return denial("policy_mismatch")
	}
	return nil
}

// AdvanceDefaultBranch records a changed GitHub default branch in the project's push policy,
// keeping the rest of the policy and retiring the old default into PreviousDefaults. It is the
// project-metadata sweep's entry to [Service.SetPolicy]: it does nothing when the project has no
// policy yet (enrollment writes one from GitHub's answer) or already names the branch, so a retry
// after a partial refresh is safe. A concurrent policy change fails the compare-and-swap with
// policy_mismatch and is retried on the next refresh.
func (s *Service) AdvanceDefaultBranch(ctx context.Context, project, branch string) error {
	if !gitref.Valid(branch) {
		return denial("policy_mismatch")
	}
	row, err := dbq.New(s.Pool).GetPushPolicy(ctx, project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var policy Policy
	if json.Unmarshal(row.Policy, &policy) != nil {
		return denial("policy_mismatch")
	}
	if policy.DefaultBranch == branch {
		return nil
	}
	policy.DefaultBranch = branch
	return s.SetPolicy(ctx, project, row.Version, policy)
}
