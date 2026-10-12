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
	// descriptionChunk is the most runes of the description one page holds; a page also stops at the
	// encoded byte budget, so a description full of characters that JSON escapes still pages.
	descriptionChunk = 16 * 1024
	// MaxDetailBytes caps the JSON body of one page.
	MaxDetailBytes = 64 * 1024
	// The generated HTTP response encoder appends one newline to the marshaled object.
	detailObjectBytes = MaxDetailBytes - 1
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
		length, dg := len(runes), digest(f.Body)
		empty := ""
		out.Description, out.DescriptionLength, out.DescriptionDigest = &empty, &length, &dg
		out.DescriptionTruncated = ptr(false)
		budget, err := pageBudget(out)
		if err != nil {
			return api.HITLMergeDetails{}, err
		}
		end := start
		for used := 0; end < len(runes) && end-start < descriptionChunk; end++ {
			cost, err := encodedLen(runes[end])
			if err != nil {
				return api.HITLMergeDetails{}, err
			}
			if used+cost > budget {
				break
			}
			used += cost
		}
		// The initial budget excludes cursor metadata. Trim against the final envelope so its
		// cursor and both truncation flags cannot push an otherwise readable page over the cap.
		for {
			if end == start && start < len(runes) {
				return api.HITLMergeDetails{}, ErrTooLarge
			}
			text := string(runes[start:end])
			out.Description = &text
			out.Truncated = end < len(runes)
			out.DescriptionTruncated = ptr(out.Truncated)
			out.NextCursor = nil
			if out.Truncated {
				c := strconv.Itoa(end)
				out.NextCursor = &c
			}
			raw, err := json.Marshal(out)
			if err != nil {
				return api.HITLMergeDetails{}, err
			}
			if len(raw) <= detailObjectBytes {
				break
			}
			if end == start {
				return api.HITLMergeDetails{}, ErrTooLarge
			}
			end--
		}
	case api.GetHITLMergeDetailsParamsSectionFiles:
		start := min(cursor, len(f.Files))
		items, end, err := fitItems(&out, start, min(limit, len(f.Files)-start), func(i int) api.MergeDetailItem {
			x := f.Files[i]
			item := api.MergeDetailItem{Filename: ptr(x.Filename), Status: ptr(x.Status), Additions: ptr(x.Additions), Deletions: ptr(x.Deletions)}
			if x.PreviousFilename != "" {
				item.PreviousFilename = ptr(x.PreviousFilename)
			}
			return item
		})
		if err != nil {
			return api.HITLMergeDetails{}, err
		}
		out.Items = &items
		if end < len(f.Files) {
			next = &end
		}
	case api.GetHITLMergeDetailsParamsSectionChecks:
		start := min(cursor, len(f.Checks))
		items, end, err := fitItems(&out, start, min(limit, len(f.Checks)-start), func(i int) api.MergeDetailItem {
			x := f.Checks[i]
			kind := api.MergeDetailItemKind(x.Kind)
			item := api.MergeDetailItem{Kind: &kind, Context: ptr(x.Context), Status: ptr(x.Status)}
			if x.AppID != 0 {
				id := int(x.AppID)
				item.AppId = &id
			}
			return item
		})
		if err != nil {
			return api.HITLMergeDetails{}, err
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
	if len(raw) > detailObjectBytes {
		return api.HITLMergeDetails{}, ErrTooLarge
	}
	return out, nil
}

// fitItems builds up to n items from start and drops items from the end until the page fits the byte
// budget, so a page of long names still pages instead of failing. It returns the items and the index
// after the last one. A single item that cannot fit is ErrTooLarge.
func fitItems(out *api.HITLMergeDetails, start, n int, item func(i int) api.MergeDetailItem) ([]api.MergeDetailItem, int, error) {
	items := make([]api.MergeDetailItem, 0, n)
	for i := start; i < start+n; i++ {
		items = append(items, item(i))
	}
	for {
		out.Items = &items
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, 0, err
		}
		if len(raw) <= detailObjectBytes {
			return items, start + len(items), nil
		}
		if len(items) <= 1 {
			return nil, 0, ErrTooLarge
		}
		items = items[:len(items)/2]
	}
}

// pageBudget is how many encoded bytes of free text a page can still carry: the cap less the page as
// it stands.
func pageBudget(out api.HITLMergeDetails) (int, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return 0, err
	}
	return detailObjectBytes - len(raw), nil
}

// encodedLen is the bytes a rune takes inside a JSON string, escapes included.
func encodedLen(r rune) (int, error) {
	raw, err := json.Marshal(string(r))
	if err != nil {
		return 0, err
	}
	return len(raw) - 2, nil
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
