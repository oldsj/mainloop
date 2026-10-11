package merge

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/oldsj/mainloop/server/internal/github"
)

// Reasons the branch requirements cannot be honoured. A requirement Mainloop cannot read, parse
// or enforce makes the evidence unavailable; it is never skipped.
const (
	ReasonProtectionFields   = "unsupported_branch_protection_fields"
	ReasonProtection         = "unsupported_branch_protection"
	ReasonStrictChecks       = "unsupported_strict_status_checks"
	ReasonOutOfDate          = "branch_out_of_date"
	ReasonRuleMalformed      = "malformed_branch_rule"
	ReasonRuleUnsupported    = "unsupported_branch_rule"
	ReasonUnknownRule        = "unsupported_branch_rule:unknown"
	ReasonCheckRuleParams    = "unsupported_required_status_checks_parameters"
	ReasonPullRequestParams  = "unsupported_pull_request_parameters"
	ReasonPullRequestUnknown = "unsupported_pull_request_parameter:unknown"
	ReasonRequiredMalformed  = "malformed_required_check"
)

// knownRuleTypes are GitHub's documented rule types. Any other type is reported as unknown, so an
// upstream identifier is never copied into a reason.
var knownRuleTypes = []string{
	"branch_name_pattern", "code_scanning", "commit_author_email_pattern", "commit_message_pattern",
	"committer_email_pattern", "copilot_code_review", "creation", "deletion", "file_extension_restriction",
	"file_path_restriction", "max_file_path_length", "max_file_size", "merge_queue", "non_fast_forward",
	"pull_request", "required_deployments", "required_linear_history", "required_signatures",
	"required_status_checks", "tag_name_pattern", "update", "workflows",
}

// RequiredCheck is a status check the base branch requires. AppID 0 accepts any app.
type RequiredCheck struct {
	Context string `json:"context"`
	AppID   int64  `json:"app_id,omitempty"`
}

type object = map[string]json.RawMessage

func objectOf(raw json.RawMessage) (object, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var o object
	return o, json.Unmarshal(raw, &o) == nil
}

// isNull reports a JSON null or an absent value.
func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	return s, len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil
}

func asBool(raw json.RawMessage) (bool, bool) {
	var b bool
	s := string(bytes.TrimSpace(raw))
	return b, (s == "true" || s == "false") && json.Unmarshal(raw, &b) == nil
}

func asInt(raw json.RawMessage) (int64, bool) {
	var n int64
	return n, json.Unmarshal(raw, &n) == nil && len(raw) > 0 && raw[0] != '"' && raw[0] != 'n'
}

func asList(raw json.RawMessage) ([]json.RawMessage, bool) {
	var l []json.RawMessage
	raw = bytes.TrimSpace(raw)
	return l, len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &l) == nil
}

// onlyKeys reports whether o has no key outside allowed.
func onlyKeys(o object, allowed ...string) bool {
	for k := range o {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// requirements is what the base branch asks of a merge, and the reasons it cannot be met.
type requirements struct {
	Required []RequiredCheck
	Reasons  []string
}

func (r *requirements) refuse(reason string) { r.Reasons = append(r.Reasons, reason) }

// require adds a required check, or refuses one that could never be satisfied.
func (r *requirements) require(context string, app int64) {
	c, ok := requiredOf(context, app)
	if !ok {
		r.refuse(ReasonRequiredMalformed)
		return
	}
	r.Required = append(r.Required, c)
}

// strictReason is the reason a strict-checks requirement gives: when GitHub says the branch is
// behind, the agent needs the instruction to update it, not the generic refusal.
func strictReason(behind bool) string {
	if behind {
		return ReasonOutOfDate
	}
	return ReasonStrictChecks
}

// requiredOf makes a required check from GitHub's context and app id. An absent or null app, and
// -1, mean any app (callers pass -1 for absent). Any other non-positive id names no app, so the
// check could never be satisfied: it is malformed.
func requiredOf(context string, app int64) (RequiredCheck, bool) {
	if app == -1 {
		app = 0
	} else if app <= 0 {
		return RequiredCheck{}, false
	}
	return RequiredCheck{Context: context, AppID: app}, context != ""
}

// readProtection judges classic branch protection as GitHub returned it. Anything it does not
// recognise exactly makes the evidence unavailable: an unknown field may carry a requirement.
func (r *requirements) readProtection(p *github.Protection, behind bool) {
	if p == nil || !p.Present {
		return
	}
	doc, ok := objectOf(p.Raw)
	if !ok || !validProtection(doc) {
		r.refuse(ReasonProtectionFields)
		return
	}
	if !isNull(doc["required_pull_request_reviews"]) || !isNull(doc["restrictions"]) {
		r.refuse(ReasonProtection)
	}
	// Squash preserves linear history; no force push, deletion or fork sync is performed. Admin
	// enforcement applies the same policy that is evaluated here.
	for _, field := range []string{"block_creations", "required_conversation_resolution", "required_signatures", "lock_branch"} {
		if flag, present := doc[field]; present {
			f, _ := objectOf(flag)
			if on, _ := asBool(f["enabled"]); on {
				r.refuse(ReasonProtection)
			}
		}
	}
	if isNull(doc["required_status_checks"]) {
		return
	}
	classic, _ := objectOf(doc["required_status_checks"])
	if strict, _ := asBool(classic["strict"]); strict {
		r.refuse(strictReason(behind))
	}
	checks, _ := asList(classic["checks"])
	for _, c := range checks {
		o, _ := objectOf(c)
		context, _ := asString(o["context"])
		app, isInt := asInt(o["app_id"])
		if !isInt {
			app = -1
		}
		r.require(context, app)
	}
	contexts, _ := asList(classic["contexts"])
	for _, c := range contexts {
		context, _ := asString(c)
		r.require(context, -1)
	}
}

// validProtection is the strict shape of classic protection: known fields only, with every flag
// present and explicit, and a required-checks object that declares what it requires.
func validProtection(doc object) bool {
	if !onlyKeys(doc, "required_status_checks", "required_pull_request_reviews", "restrictions", "url", "enabled", "name",
		"protection_url", "enforce_admins", "required_linear_history", "allow_force_pushes", "allow_deletions",
		"allow_fork_syncing", "block_creations", "required_conversation_resolution", "required_signatures", "lock_branch") {
		return false
	}
	if _, ok := doc["required_status_checks"]; !ok {
		return false
	}
	for _, k := range []string{"required_pull_request_reviews", "restrictions"} {
		if v, ok := doc[k]; ok && !isNull(v) {
			if _, obj := objectOf(v); !obj {
				return false
			}
		}
	}
	for _, k := range []string{"url", "name", "protection_url"} {
		if v, ok := doc[k]; ok && !isNull(v) {
			if _, str := asString(v); !str {
				return false
			}
		}
	}
	if v, ok := doc["enabled"]; ok {
		if _, b := asBool(v); !b {
			return false
		}
	}
	for _, k := range []string{"enforce_admins", "required_linear_history", "allow_force_pushes", "allow_deletions",
		"allow_fork_syncing", "block_creations", "required_conversation_resolution", "required_signatures", "lock_branch"} {
		v, ok := doc[k]
		if !ok {
			continue
		}
		flag, obj := objectOf(v)
		if !obj || !onlyKeys(flag, "enabled", "url") {
			return false
		}
		if _, b := asBool(flag["enabled"]); !b {
			return false
		}
		if u, has := flag["url"]; has && !isNull(u) {
			if _, str := asString(u); !str {
				return false
			}
		}
	}
	rsc := doc["required_status_checks"]
	if isNull(rsc) {
		return true
	}
	classic, obj := objectOf(rsc)
	if !obj || !onlyKeys(classic, "checks", "contexts", "strict", "url", "contexts_url", "enforcement_level") {
		return false
	}
	if s, has := classic["strict"]; has {
		if _, b := asBool(s); !b {
			return false
		}
	}
	for _, k := range []string{"url", "contexts_url", "enforcement_level"} {
		if v, has := classic[k]; has && !isNull(v) {
			if _, str := asString(v); !str {
				return false
			}
		}
	}
	checks, ok := asList(classic["checks"])
	if !ok {
		return false
	}
	for _, c := range checks {
		o, obj := objectOf(c)
		if !obj || !onlyKeys(o, "context", "app_id") {
			return false
		}
		if s, str := asString(o["context"]); !str || s == "" || len(s) > 512 {
			return false
		}
		app, has := o["app_id"]
		if !has {
			return false
		}
		if _, isInt := asInt(app); !isInt && !isNull(app) {
			return false
		}
	}
	contexts, ok := asList(classic["contexts"])
	if !ok {
		return false
	}
	for _, c := range contexts {
		if _, str := asString(c); !str {
			return false
		}
	}
	return true
}

// readRules judges the active ruleset rules. A rule other than the few that need nothing from a
// squash merge is refused: an unfamiliar rule may itself require checks.
func (r *requirements) readRules(rules []github.BranchRule, behind bool) {
	for _, rule := range rules {
		switch {
		case rule.Malformed:
			r.refuse(ReasonRuleMalformed)
		case rule.Type == "required_status_checks" && rule.Parameters != nil:
			r.readCheckRule(rule.Parameters, behind)
		case rule.Type == "pull_request":
			r.readPullRequestRule(rule.Parameters)
		case rule.Type == "deletion" || rule.Type == "non_fast_forward" || rule.Type == "required_linear_history":
		case rule.Type == "merge_queue":
			r.refuse(ReasonMergeQueue)
		case slices.Contains(knownRuleTypes, rule.Type):
			r.refuse(ReasonRuleUnsupported + ":" + rule.Type)
		default:
			r.refuse(ReasonUnknownRule)
		}
	}
}

func (r *requirements) readCheckRule(params json.RawMessage, behind bool) {
	o, ok := objectOf(params)
	if !ok || !onlyKeys(o, "required_status_checks", "strict_required_status_checks_policy", "do_not_enforce_on_create") {
		r.refuse(ReasonCheckRuleParams)
		return
	}
	strict, strictOK := asBool(o["strict_required_status_checks_policy"])
	list, listOK := asList(o["required_status_checks"])
	if v, has := o["do_not_enforce_on_create"]; has {
		if _, b := asBool(v); !b {
			listOK = false
		}
	}
	if !strictOK || !listOK {
		r.refuse(ReasonCheckRuleParams)
		return
	}
	var found []RequiredCheck
	var malformed bool
	for _, c := range list {
		item, obj := objectOf(c)
		context, str := asString(item["context"])
		if !obj || !onlyKeys(item, "context", "integration_id") || !str || context == "" || len(context) > 512 {
			r.refuse(ReasonCheckRuleParams)
			return
		}
		app := int64(-1)
		if v, has := item["integration_id"]; has && !isNull(v) {
			n, isInt := asInt(v)
			if !isInt {
				r.refuse(ReasonCheckRuleParams)
				return
			}
			app = n
		}
		check, ok := requiredOf(context, app)
		malformed = malformed || !ok
		found = append(found, check)
	}
	if strict {
		r.refuse(strictReason(behind))
	}
	if malformed {
		r.refuse(ReasonRequiredMalformed)
	}
	r.Required = append(r.Required, found...)
}

// readPullRequestRule refuses anything that could require a review or block the squash. The
// squash goes through the pull request already, so the rule's existence is not itself a reason.
func (r *requirements) readPullRequestRule(params json.RawMessage) {
	o, ok := objectOf(params)
	if !ok {
		r.refuse(ReasonPullRequestParams)
		return
	}
	known := []string{"required_approving_review_count", "dismiss_stale_reviews_on_push", "require_code_owner_review",
		"require_last_push_approval", "required_review_thread_resolution", "allowed_merge_methods", "required_reviewers",
		"require_extra_approval_for_unattributed_changes"}
	count, countOK := asInt(o["required_approving_review_count"])
	bools := map[string]bool{}
	valid := countOK && count >= 0
	for _, k := range []string{"dismiss_stale_reviews_on_push", "require_code_owner_review", "require_last_push_approval", "required_review_thread_resolution"} {
		b, isBool := asBool(o[k])
		valid = valid && isBool
		bools[k] = b
	}
	if v, has := o["require_extra_approval_for_unattributed_changes"]; has {
		_, isBool := asBool(v)
		valid = valid && isBool
	}
	var methods []string
	hasMethods := false
	if v, has := o["allowed_merge_methods"]; has && !isNull(v) {
		list, isList := asList(v)
		valid = valid && isList
		hasMethods = true
		for _, m := range list {
			s, isStr := asString(m)
			valid = valid && isStr
			methods = append(methods, s)
		}
	}
	var reviewers []json.RawMessage
	if v, has := o["required_reviewers"]; has {
		var isList bool
		reviewers, isList = asList(v)
		valid = valid && isList
	}
	if !valid {
		r.refuse(ReasonPullRequestParams)
		return
	}
	switch {
	case !onlyKeys(o, known...):
		r.refuse(ReasonPullRequestUnknown)
	case count != 0:
		r.refuse(ReasonPullRequestParams + ":required_approving_review_count")
	case bools["dismiss_stale_reviews_on_push"]:
		r.refuse(ReasonPullRequestParams + ":dismiss_stale_reviews_on_push")
	case bools["require_code_owner_review"]:
		r.refuse(ReasonPullRequestParams + ":require_code_owner_review")
	case bools["require_last_push_approval"]:
		r.refuse(ReasonPullRequestParams + ":require_last_push_approval")
	case bools["required_review_thread_resolution"]:
		r.refuse(ReasonPullRequestParams + ":required_review_thread_resolution")
	case len(reviewers) != 0:
		r.refuse(ReasonPullRequestParams + ":required_reviewers")
	case hasMethods && !slices.Contains(methods, "squash"):
		r.refuse(ReasonPullRequestParams + ":allowed_merge_methods")
	}
}
