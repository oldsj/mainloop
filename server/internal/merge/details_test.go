package merge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/merge"
)

func TestDetailsPageThroughFilesChecksAndDescription(t *testing.T) {
	e := newEnv(t)
	var files []githubtest.File
	for i := range 120 {
		files = append(files, githubtest.File{Filename: fmt.Sprintf("server/f%03d.go", i), Additions: 1})
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Files = files
		r.PullRequests[0].Body = strings.Repeat("é", 40000)
	})
	e.policy("approval")
	ctx := context.Background()
	v, err := e.svc.Merge(ctx, e.input("prep"))
	if err != nil {
		t.Fatal(err)
	}
	e.consent(v.ProposalID, "call", nil)
	card := fmt.Sprintf("req-%s-%s", v.ProposalID[:6], "call")
	get := func(section api.GetHITLMergeDetailsParamsSection, cursor, limit int) (api.HITLMergeDetails, error) {
		return e.svc.Details(ctx, merge.DetailsInput{OwnerID: owner, HITLRequestID: card, ProposalID: v.ProposalID, Section: section, Cursor: cursor, Limit: limit})
	}

	var seen []string
	cursor := 0
	for pages := 0; ; pages++ {
		page, err := get(api.GetHITLMergeDetailsParamsSectionFiles, cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		if page.Items == nil || len(*page.Items) > 50 || page.ProposalId != v.ProposalID || page.SummaryDigest != v.Digest {
			t.Fatalf("page = %+v", page)
		}
		for _, it := range *page.Items {
			seen = append(seen, *it.Filename)
		}
		if page.NextCursor == nil {
			if page.Truncated || pages != 2 {
				t.Fatalf("last page: truncated %v after %d pages", page.Truncated, pages)
			}
			break
		}
		if !page.Truncated {
			t.Fatal("a page with a next cursor must be marked truncated")
		}
		cursor, _ = strconv.Atoi(*page.NextCursor)
	}
	if len(seen) != 120 || seen[0] != "server/f000.go" || seen[119] != "server/f119.go" {
		t.Fatalf("saw %d files", len(seen))
	}

	checks, err := get(api.GetHITLMergeDetailsParamsSectionChecks, 0, 50)
	if err != nil || checks.Items == nil || len(*checks.Items) != 1 || *(*checks.Items)[0].Context != "ci" {
		t.Fatalf("checks = %+v, %v", checks, err)
	}

	desc, err := get(api.GetHITLMergeDetailsParamsSectionDescription, 0, 50)
	if err != nil || desc.Description == nil || desc.DescriptionTruncated == nil || !*desc.DescriptionTruncated || *desc.DescriptionLength != 40000 || desc.NextCursor == nil {
		t.Fatalf("description = %+v, %v", desc, err)
	}
	next, _ := strconv.Atoi(*desc.NextCursor)
	rest, err := get(api.GetHITLMergeDetailsParamsSectionDescription, next, 50)
	if err != nil || rest.NextCursor == nil {
		t.Fatalf("rest = %+v, %v", rest, err)
	}
	if *desc.DescriptionDigest != *rest.DescriptionDigest {
		t.Fatal("the description digest changed between pages")
	}
}

func TestDetailsOnlyFromACardThatNamesTheProposal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.policy("approval")
	v, err := e.svc.Merge(ctx, e.input("prep"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.svc.Merge(ctx, e.input("prep2"))
	if err != nil {
		t.Fatal(err)
	}
	e.consent(v.ProposalID, "call", nil)
	card := fmt.Sprintf("req-%s-%s", v.ProposalID[:6], "call")
	in := merge.DetailsInput{OwnerID: owner, HITLRequestID: card, ProposalID: other.ProposalID, Section: api.GetHITLMergeDetailsParamsSectionFiles}
	if _, err := e.svc.Details(ctx, in); !errors.Is(err, merge.ErrNotFound) {
		t.Fatalf("another proposal through this card = %v", err)
	}
	in.ProposalID = v.ProposalID
	in.OwnerID = "someone-else"
	if _, err := e.svc.Details(ctx, in); !errors.Is(err, merge.ErrNotFound) {
		t.Fatalf("foreign owner = %v", err)
	}
	in.OwnerID, in.HITLRequestID = owner, "missing"
	if _, err := e.svc.Details(ctx, in); !errors.Is(err, merge.ErrNotFound) {
		t.Fatalf("unknown card = %v", err)
	}
}

func TestDetailsAPIMapsErrors(t *testing.T) {
	e := newEnv(t)
	a := &merge.MergeAPI{Svc: e.svc, OwnerID: owner}
	res, err := a.GetHITLMergeDetails(context.Background(), api.GetHITLMergeDetailsRequestObject{
		RequestId: "missing", ProposalId: "p", Params: api.GetHITLMergeDetailsParams{Section: api.GetHITLMergeDetailsParamsSectionFiles},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(api.GetHITLMergeDetails404JSONResponse); !ok {
		t.Fatalf("res = %T", res)
	}
	if _, err := (&merge.MergeAPI{}).GetHITLMergeDetails(context.Background(), api.GetHITLMergeDetailsRequestObject{}); err == nil {
		t.Fatal("a nil service answered")
	}
}

func TestReviewDescriptionPageRetryCanShrink(t *testing.T) {
	// A description of characters that JSON escapes to six bytes each used to fill a page past the
	// cap at any limit. A page now stops at the encoded budget, and paging reads the whole text.
	for _, limit := range []int{50, 1} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			e := newEnv(t)
			body := strings.Repeat("<", 16*1024)
			e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].Body = body })
			e.policy("approval")
			ctx := context.Background()
			v, err := e.svc.Merge(ctx, e.input("prep"))
			if err != nil {
				t.Fatal(err)
			}
			e.consent(v.ProposalID, "call", nil)
			card := fmt.Sprintf("req-%s-%s", v.ProposalID[:6], "call")
			var got strings.Builder
			cursor := 0
			for pages := 0; pages < 20; pages++ {
				page, err := e.svc.Details(ctx, merge.DetailsInput{OwnerID: owner, HITLRequestID: card, ProposalID: v.ProposalID,
					Section: api.GetHITLMergeDetailsParamsSectionDescription, Cursor: cursor, Limit: limit})
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				raw, _ := json.Marshal(page)
				if len(raw) > merge.MaxDetailBytes {
					t.Fatalf("page %d is %d bytes", pages, len(raw))
				}
				rec := httptest.NewRecorder()
				if err := (api.GetHITLMergeDetails200JSONResponse(page)).VisitGetHITLMergeDetailsResponse(rec); err != nil {
					t.Fatal(err)
				}
				if rec.Body.Len() > merge.MaxDetailBytes {
					t.Fatalf("HTTP page %d is %d bytes, cap %d", pages, rec.Body.Len(), merge.MaxDetailBytes)
				}
				got.WriteString(*page.Description)
				if page.NextCursor == nil {
					if got.String() != body {
						t.Fatalf("read %d of %d characters", got.Len(), len(body))
					}
					return
				}
				cursor, _ = strconv.Atoi(*page.NextCursor)
			}
			t.Fatal("paging did not finish")
		})
	}
}

func TestDetailsItemPagesShrinkToTheByteBudget(t *testing.T) {
	e := newEnv(t)
	var files []githubtest.File
	for i := range 50 {
		files = append(files, githubtest.File{Filename: fmt.Sprintf("%02d-", i) + strings.Repeat("a", 3000), Additions: 1})
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].Files = files })
	e.policy("approval")
	ctx := context.Background()
	v, err := e.svc.Merge(ctx, e.input("prep"))
	if err != nil {
		t.Fatal(err)
	}
	e.consent(v.ProposalID, "call", nil)
	card := fmt.Sprintf("req-%s-%s", v.ProposalID[:6], "call")
	seen, cursor := 0, 0
	for range 60 {
		page, err := e.svc.Details(ctx, merge.DetailsInput{OwnerID: owner, HITLRequestID: card, ProposalID: v.ProposalID,
			Section: api.GetHITLMergeDetailsParamsSectionFiles, Cursor: cursor, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		seen += len(*page.Items)
		if page.NextCursor == nil {
			if seen != 50 {
				t.Fatalf("saw %d files", seen)
			}
			return
		}
		cursor, _ = strconv.Atoi(*page.NextCursor)
	}
	t.Fatal("paging did not finish")
}
