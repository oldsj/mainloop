package merge

import (
	"slices"
	"time"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/api"
)

const (
	excerptRunes = 320
	previewPaths = 5
)

// summaryOf is the presentation of a proposal: the contract's MergeSummary, which the owner's card
// shows and the agent's prepare call returns. It holds bounded excerpts; the details endpoint pages
// through the rest.
func summaryOf(proposalID string, f Facts) api.MergeSummary {
	ready := len(f.Unavailable) == 0
	availability := api.MergeSummaryAvailabilityReady
	if !ready {
		availability = api.MergeSummaryAvailabilityUnavailable
	}
	policy := api.MergeSummaryPolicy(f.Policy)

	reasons := []api.MergeApprovalReason{}
	if f.Policy == string(api.MergeSummaryPolicyApproval) {
		var r api.MergeApprovalReason
		_ = r.FromMergeApprovalProjectPolicy(api.MergeApprovalProjectPolicy{Type: api.ProjectPolicy})
		reasons = append(reasons, r)
	}
	matches := []string{}
	for _, m := range f.Protected {
		var r api.MergeApprovalReason
		_ = r.FromMergeApprovalProtectedPath(api.MergeApprovalProtectedPath{Glob: m.Glob, Path: m.Path, Type: api.ProtectedPath})
		reasons = append(reasons, r)
		matches = append(matches, m.Path)
	}
	slices.Sort(matches)
	matches = slices.Compact(matches)

	paths := make([]string, 0, len(f.Files))
	for _, x := range f.Files {
		paths = append(paths, x.Filename)
	}
	slices.Sort(paths)
	preview := paths[:min(len(paths), previewPaths)]
	if preview == nil {
		preview = []string{}
	}

	excerpt, truncated := excerptOf(f.Body)
	bodyLen := utf8.RuneCountInString(f.Body)
	bodyDigest := digest(f.Body)
	capturedAt := f.CapturedAt.UTC().Format(time.RFC3339)
	c := countChecks(f.Checks)
	ci := &api.MergeCISummary{
		CapturedAt: &capturedAt, Complete: c.Pending == 0, FailedCount: c.Failed,
		GreenAtPreparation: green(c) && ready, InventoryDigest: digest(f.Checks),
		PassedCount: c.Passed, PendingCount: c.Pending, ResultCount: c.Results,
	}
	if f.IgnoredSuites > 0 {
		ci.IgnoredSuiteCount = &f.IgnoredSuites
	}
	unavailable := f.Unavailable
	if unavailable == nil {
		unavailable = []string{}
	}
	fileCount, globs, polVer, prNumber := len(f.Files), f.GlobsVersion, f.PolicyVer, f.PRNumber

	return api.MergeSummary{
		Additions: &f.Additions, ApprovalReasons: reasons, Availability: availability,
		Base: &f.Base, BaseSha: &f.BaseSHA, Ci: ci, Deletions: &f.Deletions,
		DescriptionDigest: &bodyDigest, DescriptionExcerpt: excerpt, DescriptionLength: &bodyLen,
		DescriptionTruncated: truncated, FileCount: &fileCount, GlobsVersion: &globs,
		Head: &f.Head, HeadSha: &f.HeadSHA, PathsDigest: &f.PathsDigest,
		PathsPreview: preview, PathsPreviewTruncated: len(paths) > previewPaths,
		Policy: &policy, PolicyVersion: &polVer, PrNumber: &prNumber, ProposalId: proposalID,
		ProtectedMatches: matches, Repository: &f.Repository, Title: &f.Title,
		UnavailableReasons: unavailable, Version: 1,
	}
}

func excerptOf(body string) (string, bool) {
	if utf8.RuneCountInString(body) <= excerptRunes {
		return body, false
	}
	runes := []rune(body)
	return string(runes[:excerptRunes]), true
}
