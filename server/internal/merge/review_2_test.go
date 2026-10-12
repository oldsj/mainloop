package merge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/merge"
)

func TestRereviewDescriptionBudgetIncludesFinalCursor(t *testing.T) {
	for suffix := 0; suffix < 6; suffix++ {
		t.Run(fmt.Sprintf("request_suffix_%d", suffix), func(t *testing.T) {
			e := newEnv(t)
			body := strings.Repeat("<", 16*1024)
			e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].Body = body })
			v := e.approvalProposal()
			request := "call" + strings.Repeat("x", suffix)
			e.consent(v.ProposalID, request, nil)
			card := fmt.Sprintf("req-%s-%s", v.ProposalID[:6], request)
			for _, limit := range []int{50, 1} {
				for _, startCursor := range []int{0, 1000} {
					t.Run(fmt.Sprintf("limit_%d_cursor_%d", limit, startCursor), func(t *testing.T) {
						var got strings.Builder
						cursor := startCursor
						for pages := 0; pages < 20; pages++ {
							page, err := e.svc.Details(context.Background(), merge.DetailsInput{
								OwnerID: owner, HITLRequestID: card, ProposalID: v.ProposalID,
								Section: api.GetHITLMergeDetailsParamsSectionDescription, Limit: limit, Cursor: cursor,
							})
							if err != nil {
								t.Fatalf("cursor=%d: valid escaped description is unreadable: %v", cursor, err)
							}
							raw, err := json.Marshal(page)
							if err != nil || len(raw) > merge.MaxDetailBytes || page.Description == nil {
								t.Fatalf("cursor=%d bytes=%d error=%v", cursor, len(raw), err)
							}
							got.WriteString(*page.Description)
							truncated := page.NextCursor != nil
							if page.DescriptionTruncated == nil || *page.DescriptionTruncated != truncated || page.Truncated != truncated {
								t.Fatalf("cursor=%d: inconsistent truncation flags", cursor)
							}
							if !truncated {
								if pages == 0 || got.String() != body[startCursor:] {
									t.Fatalf("read %d of %d characters in %d pages", got.Len(), len(body)-startCursor, pages+1)
								}
								return
							}
							next, err := strconv.Atoi(*page.NextCursor)
							if err != nil || next <= cursor {
								t.Fatalf("cursor=%d next=%v error=%v", cursor, page.NextCursor, err)
							}
							cursor = next
						}
						t.Fatal("paging did not finish")
					})
				}
			}
		})
	}
}

func TestRereviewApprovalProjectScopeMustHoldUntilDispatch(t *testing.T) {
	for _, when := range []string{"before_completion_sweep", "after_evaluation_began", "during_evidence"} {
		t.Run(when, func(t *testing.T) {
			e := newEnv(t)
			var proposal string
			if when == "before_completion_sweep" {
				v := e.approvalProposal()
				proposal = v.ProposalID
				e.consent(proposal, "good", nil)
				e.exec(`UPDATE sessions SET project_id = NULL WHERE id = 's1'`)
				_, err := e.svc.MergeApproved(context.Background(), merge.ApprovedInput{
					OwnerID: owner, SessionID: "s1", ActorProjectID: "", ProposalID: proposal, RequestID: "good",
				})
				if refusalOf(err) != "forbidden" {
					t.Fatalf("direct call must be forbidden: %v", err)
				}
				if err := e.svc.Complete(context.Background()); err != nil {
					t.Fatal(err)
				}
				if state := e.state(proposal); state != dbstate.MergePrepared || e.puts() != 0 {
					t.Fatalf("completion without project authority: state=%s PUTs=%d", state, e.puts())
				}
				return
			} else {
				proposal = e.freezeApproved()
				if when == "during_evidence" {
					e.fake.Before("GET", "/repos/o/n/commits/"+tip+"/statuses", func() {
						e.exec(`UPDATE sessions SET project_id = NULL WHERE id = 's1'`)
					})
				} else {
					e.exec(`UPDATE sessions SET project_id = NULL WHERE id = 's1'`)
				}
			}
			e.run()
			if state := e.settled(proposal); state != dbstate.MergeBlocked || e.detail(proposal) != "consent_project_changed" || e.puts() != 0 {
				t.Fatalf("lost project authority but state=%s detail=%s PUTs=%d", state, e.detail(proposal), e.puts())
			}
		})
	}
}

func TestRereviewApprovalSessionProjectMustMatchProposal(t *testing.T) {
	for _, when := range []string{"before_completion_sweep", "after_evaluation_began"} {
		t.Run(when, func(t *testing.T) {
			e := newEnv(t)
			e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url)
				VALUES ('p2', $1, 'o', 'other', 'o/other', 'https://x')`, owner)
			var proposal string
			if when == "before_completion_sweep" {
				v := e.approvalProposal()
				proposal = v.ProposalID
				e.consent(proposal, "good", nil)
			} else {
				proposal = e.freezeApproved()
			}
			e.exec(`UPDATE sessions SET project_id = 'p2' WHERE id = 's1'`)
			if when == "before_completion_sweep" {
				if err := e.svc.Complete(context.Background()); err != nil {
					t.Fatal(err)
				}
				if state := e.state(proposal); state != dbstate.MergePrepared || e.puts() != 0 {
					t.Fatalf("completion from another project: state=%s PUTs=%d", state, e.puts())
				}
			} else {
				e.stops(proposal, dbstate.MergeBlocked, "consent_project_changed")
			}
		})
	}
}

func TestRereviewHeadChangeAtPUTIsGuarded(t *testing.T) {
	e := newEnv(t)
	proposal := e.freezeApproved()
	e.fake.Before("PUT", mergePath, func() {
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.PullRequests[0].HeadSHA = moved
			r.Branches["feature"] = moved
		})
	})
	e.run()
	if state := e.settled(proposal); state != dbstate.MergeBlocked || e.detail(proposal) != "merge_refused_409" || e.puts() != 1 || e.pr().Merged {
		t.Fatalf("state=%s detail=%s PUTs=%d remote merged=%v", state, e.detail(proposal), e.puts(), e.pr().Merged)
	}
	for _, request := range e.fake.Requests() {
		if request.Method == "PUT" && request.Path == mergePath {
			var sent struct {
				SHA string `json:"sha"`
			}
			if err := json.Unmarshal([]byte(request.Body), &sent); err != nil || sent.SHA != tip {
				t.Fatalf("merge did not carry the approved head: sha=%q error=%v", sent.SHA, err)
			}
		}
	}
}
