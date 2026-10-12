package merge

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/oldsj/mainloop/server/internal/github"
)

// Limits of the merge inventory. GitHub lists at most 300 changed files for a comparison, so a list
// at the cap cannot prove the inventory is complete; the default branch may be this many commits
// ahead of the merge base before the pull request must be updated.
const (
	mergeFilesLimit  = github.CompareFilesLimit
	baseCommitsLimit = 1000
	comparePerPage   = 100
)

// Reasons the merge inventory cannot be established. Each tells the agent to update the branch.
const (
	ReasonInventoryTooLarge = "merge_inventory_too_many_files"
	ReasonInventoryBehind   = "merge_inventory_default_branch_too_far_ahead"
	ReasonInventoryMerges   = "merge_inventory_default_branch_has_merge_commits"
	ReasonInventoryMoved    = "merge_inventory_paths_moved_by_default_branch"
)

// errInventory means GitHub's comparisons disagree with each other or with the pinned commits. It is
// not a refusal of the pull request: the read is retried.
var errInventory = errors.New("merge inventory: comparisons are inconsistent")

// inventory is the result of the compare-based reading.
type inventory struct {
	MergeBase string
	Files     []FileFact
	// Reason is set when the squash's paths cannot be established; the evidence is then unavailable.
	Reason string
}

func fileFacts(files []github.PullFile) []FileFact {
	out := make([]FileFact, 0, len(files))
	for _, x := range files {
		out = append(out, FileFact{Filename: x.Filename, PreviousFilename: x.PreviousFilename, Status: x.Status, Additions: x.Additions, Deletions: x.Deletions})
	}
	return out
}

// comparedFiles is the file list of a comparison. A comparison that did not list files, or listed
// a path twice, is inconsistent; one at the cap is too large to trust; one with a path that cannot
// be matched safely is refused like an invalid pull request path.
func comparedFiles(c *github.Comparison) (files []FileFact, reason string, err error) {
	if !c.FilesListed {
		return nil, "", errInventory
	}
	if len(c.Files) >= mergeFilesLimit {
		return nil, ReasonInventoryTooLarge, nil
	}
	files = fileFacts(c.Files)
	if _, perr := protectedMatches(files); perr != nil {
		return nil, ReasonInvalidPath, nil
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Filename] {
			return nil, "", errInventory
		}
		seen[f.Filename] = true
	}
	return files, "", nil
}

// mergeInventory reads the paths a squash of head onto base would change. Both inventories come
// from documented three-dot comparisons: the head's changes since the merge base and, when the
// default branch has moved on, the default branch's. A rename or removal that could carry a change
// outside the head inventory, a default branch that is not linear since the merge base, or a
// comparison that cannot be read completely is refused, never approximated.
func mergeInventory(ctx context.Context, gh GitHub, repo github.Repo, baseSHA, headSHA string) (*inventory, error) {
	headSide, err := gh.Compare(ctx, repo, baseSHA, headSHA, 1, 1)
	if err != nil {
		return nil, readErr("compare head with default branch", err)
	}
	mergeBase := headSide.MergeBaseSHA
	current := mergeBase == baseSHA
	want := "diverged"
	if current {
		want = "ahead"
	}
	if headSide.BaseSHA != baseSHA || headSide.AheadBy < 1 || headSide.Status != want || (headSide.BehindBy == 0) != current {
		return nil, errInventory
	}
	headFiles, reason, err := comparedFiles(headSide)
	if err != nil || reason != "" {
		return &inventory{MergeBase: mergeBase, Reason: reason}, err
	}
	if current {
		return &inventory{MergeBase: mergeBase, Files: headFiles}, nil
	}
	if headSide.BehindBy > baseCommitsLimit {
		return &inventory{MergeBase: mergeBase, Reason: ReasonInventoryBehind}, nil
	}

	commits := map[string]github.CompareCommit{}
	var baseFiles []FileFact
	var baseSide *github.Comparison
	for page := 1; page <= baseCommitsLimit/comparePerPage; page++ {
		baseSide, err = gh.Compare(ctx, repo, mergeBase, baseSHA, comparePerPage, page)
		if err != nil {
			return nil, readErr("compare default branch with merge base", err)
		}
		if baseSide.Status != "ahead" || baseSide.BehindBy != 0 || baseSide.AheadBy != headSide.BehindBy ||
			baseSide.BaseSHA != mergeBase || baseSide.MergeBaseSHA != mergeBase {
			return nil, errInventory
		}
		if baseFiles == nil {
			var r string
			if baseFiles, r, err = comparedFiles(baseSide); err != nil || r != "" {
				return &inventory{MergeBase: mergeBase, Reason: r}, err
			}
		}
		for _, c := range baseSide.Commits {
			if _, dup := commits[c.SHA]; dup {
				return nil, errInventory
			}
			commits[c.SHA] = c
		}
		// Stop once every commit is listed; a full page then needs no empty follow-up read.
		if len(commits) >= baseSide.AheadBy || len(baseSide.Commits) < comparePerPage {
			break
		}
	}
	if _, ok := commits[baseSHA]; len(commits) != baseSide.AheadBy || !ok {
		return nil, errInventory
	}
	// One merge base is what makes the three-way argument exact. A linear range from it to the
	// default branch's tip leaves no other common ancestor.
	for _, c := range commits {
		direct := len(c.Parents) == 1 && c.Parents[0] == mergeBase
		_, inRange := commits[firstParent(c)]
		if !direct && (len(c.Parents) != 1 || !inRange) {
			return &inventory{MergeBase: mergeBase, Reason: ReasonInventoryMerges}, nil
		}
	}
	if moved := slices.Concat(redirectedPaths(baseFiles, headFiles, true), redirectedPaths(headFiles, baseFiles, false)); len(moved) > 0 {
		return &inventory{MergeBase: mergeBase, Reason: ReasonInventoryMoved}, nil
	}
	return &inventory{MergeBase: mergeBase, Files: headFiles}, nil
}

func firstParent(c github.CompareCommit) string {
	if len(c.Parents) == 0 {
		return ""
	}
	return c.Parents[0]
}

// parentDirs are the non-root ancestor directories of a repository path, nearest first.
func parentDirs(path string) []string {
	parts := strings.Split(path, "/")
	parts = parts[:len(parts)-1]
	out := make([]string, 0, len(parts))
	for i := len(parts); i > 0; i-- {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}

// redirectedPaths lists the paths a three-way merge may move because renaming moved them. Sources
// are the paths renaming removed or renamed away, since GitHub's compare and Git's merge can
// disagree on rename pairing. With exact, a path of other that is a source can be carried to a path
// neither inventory names. A path other adds can follow a directory rename into another directory;
// Git renames only a directory that no longer exists on the renaming side, so a directory still
// proven to hold a file there is safe. The root directory always exists.
func redirectedPaths(renaming, other []FileFact, exact bool) []string {
	sources := map[string]bool{}
	for _, f := range renaming {
		switch f.Status {
		case "renamed":
			sources[f.PreviousFilename] = true
		case "removed":
			sources[f.Filename] = true
		}
	}
	if len(sources) == 0 {
		return nil
	}
	remaining := map[string]bool{}
	for _, f := range renaming {
		if f.Status != "removed" {
			remaining[f.Filename] = true
		}
	}
	for _, f := range other {
		var existed string
		switch f.Status {
		case "renamed", "copied":
			existed = f.PreviousFilename
		case "added":
		default:
			existed = f.Filename
		}
		if existed != "" && !sources[existed] {
			remaining[existed] = true
		}
	}
	sourceDirs, remainingDirs := map[string]bool{}, map[string]bool{}
	for p := range sources {
		for _, d := range parentDirs(p) {
			sourceDirs[d] = true
		}
	}
	for p := range remaining {
		for _, d := range parentDirs(p) {
			remainingDirs[d] = true
		}
	}
	found := map[string]bool{}
	for _, f := range other {
		if exact {
			for _, n := range []string{f.Filename, f.PreviousFilename} {
				if sources[n] {
					found[n] = true
				}
			}
		}
		if f.Status == "added" || f.Status == "renamed" || f.Status == "copied" {
			for _, d := range parentDirs(f.Filename) {
				if sourceDirs[d] && !remainingDirs[d] {
					found[f.Filename] = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
