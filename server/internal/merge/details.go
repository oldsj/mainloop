package merge

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

const (
	// MaxDetailItems is the most items one page of details holds.
	MaxDetailItems = 50
	// descriptionChunk is how many runes of the description one page holds.
	descriptionChunk = 16 * 1024
	// MaxDetailBytes caps the JSON body of one page.
	MaxDetailBytes = 64 * 1024
)

// ErrTooLarge means a page would exceed MaxDetailBytes.
var ErrTooLarge = errors.New("merge: details page is too large")

// DetailsInput asks for one page of a proposal's details, as shown on a HITL card.
type DetailsInput struct {
	OwnerID, HITLRequestID, ProposalID string
	Section                            api.GetHITLMergeDetailsParamsSection
	Cursor, Limit                      int
}

// Details returns one page of the frozen facts of a proposal. The proposal must be the one the
// owner's HITL card asks about, so a card cannot be used to read another proposal.
func (s *Service) Details(ctx context.Context, in DetailsInput) (api.HITLMergeDetails, error) {
	q := dbq.New(s.Pool)
	req, err := q.GetHITLRequest(ctx, dbq.GetHITLRequestParams{ID: in.HITLRequestID, OwnerID: in.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.HITLMergeDetails{}, ErrNotFound
	}
	if err != nil {
		return api.HITLMergeDetails{}, err
	}
	if !cardNames(req.Payload, in.ProposalID) {
		return api.HITLMergeDetails{}, ErrNotFound
	}
	row, err := q.GetMergeView(ctx, dbq.GetMergeViewParams{ID: in.ProposalID, OwnerID: in.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.HITLMergeDetails{}, ErrNotFound
	}
	if err != nil {
		return api.HITLMergeDetails{}, err
	}
	var f Facts
	if err := json.Unmarshal(row.Facts, &f); err != nil {
		return api.HITLMergeDetails{}, err
	}
	limit := in.Limit
	if limit <= 0 || limit > MaxDetailItems {
		limit = MaxDetailItems
	}
	cursor := max(in.Cursor, 0)

	capturedAt := f.CapturedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	out := api.HITLMergeDetails{
		Base: &f.Base, BaseSha: &f.BaseSHA, CapturedAt: &capturedAt, Cursor: cursor,
		Head: &f.Head, HeadSha: &f.HeadSHA, ProposalId: in.ProposalID, RequestId: in.HITLRequestID,
		Section: api.HITLMergeDetailsSection(in.Section), SummaryDigest: row.SummaryDigest,
	}
	var next *int
	switch in.Section {
	case api.GetHITLMergeDetailsParamsSectionDescription:
		runes := []rune(f.Body)
		start := min(cursor, len(runes))
		end := min(start+descriptionChunk, len(runes))
		text, length, dg, trunc := string(runes[start:end]), len(runes), digest(f.Body), end < len(runes)
		out.Description, out.DescriptionLength, out.DescriptionDigest, out.DescriptionTruncated = &text, &length, &dg, &trunc
		if trunc {
			next = &end
		}
	case api.GetHITLMergeDetailsParamsSectionFiles:
		items := []api.MergeDetailItem{}
		start := min(cursor, len(f.Files))
		end := min(start+limit, len(f.Files))
		for _, x := range f.Files[start:end] {
			item := api.MergeDetailItem{Filename: ptr(x.Filename), Status: ptr(x.Status), Additions: ptr(x.Additions), Deletions: ptr(x.Deletions)}
			if x.PreviousFilename != "" {
				item.PreviousFilename = ptr(x.PreviousFilename)
			}
			items = append(items, item)
		}
		out.Items = &items
		if end < len(f.Files) {
			next = &end
		}
	case api.GetHITLMergeDetailsParamsSectionChecks:
		items := []api.MergeDetailItem{}
		start := min(cursor, len(f.Checks))
		end := min(start+limit, len(f.Checks))
		for _, x := range f.Checks[start:end] {
			kind := api.MergeDetailItemKind(x.Kind)
			item := api.MergeDetailItem{Kind: &kind, Context: ptr(x.Context), Status: ptr(x.Status)}
			if x.AppID != 0 {
				id := int(x.AppID)
				item.AppId = &id
			}
			items = append(items, item)
		}
		out.Items = &items
		if end < len(f.Checks) {
			next = &end
		}
	default:
		return api.HITLMergeDetails{}, ErrNotFound
	}
	if next != nil {
		c := strconv.Itoa(*next)
		out.NextCursor, out.Truncated = &c, true
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return api.HITLMergeDetails{}, err
	}
	if len(raw) > MaxDetailBytes {
		return api.HITLMergeDetails{}, ErrTooLarge
	}
	return out, nil
}

// cardNames reports whether a stored tool-approval request is about this proposal.
func cardNames(payload json.RawMessage, proposalID string) bool {
	var req api.ToolApprovalRequest
	if json.Unmarshal(payload, &req) != nil {
		return false
	}
	for _, t := range req.Tools {
		if pid, _ := t.Args["proposal_id"].(string); pid == proposalID {
			return true
		}
	}
	return false
}
