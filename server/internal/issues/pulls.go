package issues

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/github"
)

// The bounds of one get_pull_request answer, chosen so the whole answer stays within what an
// agent's MCP client takes from one tool result (about 25k tokens in Claude Code). Patches share one budget across files, in the order
// GitHub lists them, so a large pull request still shows every file's name and counts and as many
// patches as fit; the caller names the rest by path in later reads.
const (
	// DiffBudget is the most patch text, in bytes, one read returns across all its files.
	DiffBudget = 32_000
	// FilePatchMax bounds one file's patch; a longer one is cut at a line break.
	FilePatchMax = 16_000
	// MaxPullFiles is the most changed files one read lists.
	MaxPullFiles = 300
	// MaxPullPaths is the most paths a read may ask patches for.
	MaxPullPaths = 50
	// PullBodyMax bounds the pull request's description.
	PullBodyMax = 8_000
	// FeedbackMax is the most review items returned; the newest are kept.
	FeedbackMax = 30
	// FeedbackBodyMax bounds one review item's text.
	FeedbackBodyMax = 1_500
)

// minPatchCut is the least budget worth cutting a patch into; below it a patch that does not fit
// whole is omitted rather than cut to a fragment.
const minPatchCut = 1_000

// pullFilesPerPage stays small because every file carries its patch and every GitHub answer is
// capped in size.
const pullFilesPerPage = 30

// Patch states of a listed file.
const (
	// PatchFull is the whole patch.
	PatchFull = "full"
	// PatchTruncated is the patch cut at FilePatchMax or at what was left of the budget.
	PatchTruncated = "truncated"
	// PatchOmitted means the budget ran out before this file: read it again by path.
	PatchOmitted = "omitted"
	// PatchNotRequested means the read named other paths.
	PatchNotRequested = "not_requested"
	// PatchUnavailable means GitHub has no patch to show: a binary file, or a diff too large.
	PatchUnavailable = "unavailable"
)

// PullRequestRead is one pull request as the reviewing agent sees it. The description, patches
// and review text are other people's words: untrusted input to the agent.
type PullRequestRead struct {
	Number int
	Title  string
	Body   string
	// BodyTruncated is set when the description was cut at PullBodyMax.
	BodyTruncated bool
	// State is open, closed or merged.
	State          string
	Draft          bool
	Author         string
	HeadRef        string
	HeadSHA        string
	BaseRef        string
	Mergeable      *bool
	MergeableState string
	Additions      int
	Deletions      int
	ChangedFiles   int
	URL            string

	Files []PullFileRead
	// FilesTruncated is set when fewer files are listed than the pull request changes.
	FilesTruncated bool
	// PathsNotFound are asked-for paths no listed file has.
	PathsNotFound []string

	// Feedback are the trusted reviews, inline review comments and conversation comments, oldest
	// first.
	Feedback []FeedbackRead
	// FeedbackOmitted counts older trusted items left out past FeedbackMax.
	FeedbackOmitted int
	// FeedbackIgnored counts items from authors who are not trusted; their text is never read.
	FeedbackIgnored int
}

// PullFileRead is one changed file with as much of its patch as the read could carry.
type PullFileRead struct {
	Path         string
	PreviousPath string
	Status       string
	Additions    int
	Deletions    int
	Patch        string
	PatchState   string
}

// FeedbackRead is one review, inline review comment or conversation comment.
type FeedbackRead struct {
	Kind          string
	Author        string
	State         string
	Path          string
	Line          int
	Body          string
	BodyTruncated bool
	URL           string
	At            time.Time
}

// GetPullRequest reads one pull request of the project's repository: its description and state,
// its changed files with patches up to [DiffBudget], and its trusted review feedback. With paths,
// only those files carry patches. CI results are read separately (check_status).
func (s *Service) GetPullRequest(ctx context.Context, ownerID, projectID string, number int, paths []string) (PullRequestRead, error) {
	repo, err := s.project(ctx, ownerID, projectID)
	if err != nil {
		return PullRequestRead{}, err
	}
	info, err := s.GitHub.PullRequest(ctx, repo, number)
	switch {
	case errors.Is(err, github.ErrNotFound):
		return PullRequestRead{}, &Refusal{"not_found", "no such pull request"}
	case err != nil:
		return PullRequestRead{}, unreadable(err)
	}
	out := pullHeader(*info)

	files, complete, err := s.pullFiles(ctx, repo, number)
	if err != nil {
		return PullRequestRead{}, unreadable(err)
	}
	out.Files, out.PathsNotFound = shapeFiles(files, paths)
	out.FilesTruncated = !complete || len(files) < info.ChangedFiles

	got, err := s.GitHub.PullRequestFeedback(ctx, repo, number, time.Time{})
	if err != nil {
		return PullRequestRead{}, unreadable(err)
	}
	out.Feedback, out.FeedbackOmitted, out.FeedbackIgnored = shapeFeedback(got)
	return out, nil
}

func unreadable(err error) error {
	return &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
}

func pullHeader(info github.PullRequestInfo) PullRequestRead {
	state := info.State
	if info.Merged {
		state = "merged"
	}
	body, cut := clip(info.Body, PullBodyMax)
	return PullRequestRead{
		Number: info.Number, Title: info.Title, Body: body, BodyTruncated: cut, State: state, Draft: info.Draft,
		Author: info.Author, HeadRef: info.Head.Ref, HeadSHA: info.Head.SHA, BaseRef: info.Base.Ref,
		Mergeable: info.Mergeable, MergeableState: info.MergeableState,
		Additions: info.Additions, Deletions: info.Deletions, ChangedFiles: info.ChangedFiles, URL: info.HTMLURL,
	}
}

// pullFiles lists up to MaxPullFiles changed files. A page GitHub answers too large to read ends
// the listing early rather than failing the read; complete is false then.
func (s *Service) pullFiles(ctx context.Context, repo github.Repo, number int) ([]github.PullFile, bool, error) {
	var files []github.PullFile
	for page := 1; len(files) < MaxPullFiles; page++ {
		got, more, err := s.GitHub.PullFiles(ctx, repo, number, page, pullFilesPerPage)
		if errors.Is(err, github.ErrResponseTooLarge) {
			return files, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		files = append(files, got...)
		if !more {
			return files, true, nil
		}
	}
	return files[:min(len(files), MaxPullFiles)], false, nil
}

// shapeFiles attaches patches to files within the budget. With paths, only those files carry a
// patch, and the paths no file has are returned.
func shapeFiles(files []github.PullFile, paths []string) ([]PullFileRead, []string) {
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = false
	}
	left := DiffBudget
	out := make([]PullFileRead, 0, len(files))
	for _, f := range files {
		r := PullFileRead{
			Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions,
		}
		_, asked := want[f.Filename]
		switch {
		case len(want) > 0 && !asked:
			r.PatchState = PatchNotRequested
		case f.Patch == "":
			r.PatchState = PatchUnavailable
		case len(f.Patch) > left && left < minPatchCut:
			// What is left would hold only a fragment: the whole patch is better read by path.
			r.PatchState = PatchOmitted
		default:
			patch, cut := clip(f.Patch, min(FilePatchMax, left))
			if cut && !strings.Contains(patch, "\n") {
				r.PatchState = PatchOmitted
				break
			}
			left -= len(patch)
			r.Patch, r.PatchState = patch, PatchFull
			if cut {
				r.PatchState = PatchTruncated
			}
		}
		if asked {
			want[f.Filename] = true
		}
		out = append(out, r)
	}
	var missing []string
	for _, p := range paths {
		if !want[p] && !slices.Contains(missing, p) {
			missing = append(missing, p)
		}
	}
	return out, missing
}

// shapeFeedback keeps the trusted, submitted feedback, newest FeedbackMax of it, oldest first. A
// review that is pending, or that only groups inline comments (COMMENTED with no text), says
// nothing of its own and is skipped.
func shapeFeedback(got []github.PRFeedback) (items []FeedbackRead, omitted, ignored int) {
	for _, f := range got {
		switch {
		case f.Kind == github.FeedbackReview && (f.State == "PENDING" || f.State == "COMMENTED" && strings.TrimSpace(f.Body) == ""):
			continue
		case !f.Trusted():
			ignored++
			continue
		}
		body, cut := clip(f.Body, FeedbackBodyMax)
		items = append(items, FeedbackRead{
			Kind: f.Kind, Author: f.Author, State: f.State, Path: f.Path, Line: f.Line,
			Body: body, BodyTruncated: cut, URL: f.URL, At: f.At,
		})
	}
	if len(items) > FeedbackMax {
		omitted = len(items) - FeedbackMax
		items = items[omitted:]
	}
	return items, omitted, ignored
}

// clip cuts s to at most n bytes, at the last line break when there is one in the kept part, and
// never inside a UTF-8 sequence. It reports whether it cut.
func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	kept := s[:n]
	if i := strings.LastIndexByte(kept, '\n'); i > 0 {
		return kept[:i+1], true
	}
	for len(kept) > 0 && !utf8.ValidString(kept) {
		kept = kept[:len(kept)-1]
	}
	return kept, true
}
