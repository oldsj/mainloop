package tasks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gitref"
)

const descriptionLimit = 16 << 10

func describe(o Observation, attempt *string, repo, branch string) *api.TaskPRDescription {
	if o.PRBody == nil || attempt == nil || o.ObservedAt.IsZero() || !utf8.ValidString(*o.PRBody) ||
		o.PRNumber <= 0 || !strings.EqualFold(o.PRRepo, repo) || o.PRHeadRef != branch ||
		(o.PRState != prOpen && o.PRState != prClosed && o.PRState != prMerged) {
		return nil
	}
	body := *o.PRBody
	end := min(len(body), descriptionLimit)
	for end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	d := &api.TaskPRDescription{
		Repository: strings.ToLower(repo), Branch: branch, PrNumber: o.PRNumber,
		AttemptId: *attempt, HeadSha: o.PRHeadSHA, Excerpt: body[:end],
		ByteLength: len(body), Digest: bodyDigest(body), Truncated: len(body) > descriptionLimit,
		FetchedAt: o.ObservedAt,
	}
	if !validDescription(d) {
		return nil
	}
	return d
}

func bodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func lowerHex(s string, length int) bool {
	if len(s) != length || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validDescription(d *api.TaskPRDescription) bool {
	repo, err := github.ParseRepo(d.Repository)
	if err != nil || repo.FullName() != d.Repository || len(d.Repository) > 200 || !gitref.ValidCheckoutRef(d.Branch) ||
		d.AttemptId == "" || len(d.AttemptId) > 100 || d.PrNumber <= 0 || !lowerHex(d.HeadSha, 40) ||
		!lowerHex(d.Digest, 64) || d.FetchedAt.IsZero() || !utf8.ValidString(d.Excerpt) ||
		strings.ContainsRune(d.Excerpt, 0) || len(d.Excerpt) > descriptionLimit || d.ByteLength < 0 ||
		d.Truncated != (d.ByteLength > descriptionLimit) {
		return false
	}
	if d.Truncated {
		return len(d.Excerpt) >= descriptionLimit-3
	}
	return d.ByteLength == len(d.Excerpt) && d.Digest == bodyDigest(d.Excerpt)
}

func storedDescription(raw json.RawMessage) *api.TaskPRDescription {
	var d *api.TaskPRDescription
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 || decoder.Decode(&d) != nil || d == nil || !validDescription(d) {
		return nil
	}
	return d
}

// Fetch time alone changes neither the task version nor its event stream.
func sameDescription(raw json.RawMessage, next *api.TaskPRDescription) bool {
	prev := storedDescription(raw)
	if prev == nil || next == nil {
		return prev == next
	}
	a, b := *prev, *next
	a.FetchedAt, b.FetchedAt = time.Time{}, time.Time{}
	return a == b
}

func descriptionFor(t dbq.Task) *api.TaskPRDescription {
	d := storedDescription(t.PrDescription)
	if d == nil || t.PrRepository == nil || d.Repository != *t.PrRepository ||
		t.CheckoutBranch == nil || d.Branch != *t.CheckoutBranch || t.PrNumber == nil ||
		d.PrNumber != int(*t.PrNumber) || t.PrHeadSha == nil || d.HeadSha != *t.PrHeadSha ||
		t.CurrentAttemptID == nil || d.AttemptId != *t.CurrentAttemptID || t.PrState == stUnknown {
		return nil
	}
	if age := time.Since(d.FetchedAt); age < 0 || age > 5*time.Minute {
		return nil
	}
	return d
}
