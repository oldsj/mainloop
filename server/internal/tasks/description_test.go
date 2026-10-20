package tasks_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func descriptionObservation(body *string) tasks.Observation {
	return tasks.Observation{
		PRNumber: 7, PRURL: "https://github.com/o/p1/pull/7", PRHeadRef: "feature", PRRepo: "O/P1",
		PRHeadSHA: strings.Repeat("a", 40), PRState: "open", PRBody: body,
		ObservedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestDescriptionBoundsAndDigest(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	for _, body := range []string{
		"", "hello", "Supported\né\t\rtext � 😀", strings.Repeat("a", 16384),
		strings.Repeat("a", 16383) + "étail", strings.Repeat("a", 16382) + "€tail",
		strings.Repeat("a", 16381) + "😀tail", strings.Repeat("a", 16384) + "\x00",
	} {
		t.Run(fmt.Sprintf("bytes_%d", len(body)), func(t *testing.T) {
			o := descriptionObservation(&body)
			r.obs.set(o, nil)
			r.project()
			got := r.view(r.own, id).Projection.PrDescription
			prefix := []byte(body)[:min(len(body), 16384)]
			for !utf8.Valid(prefix) {
				prefix = prefix[:len(prefix)-1]
			}
			if got == nil || got.Excerpt != string(prefix) || got.ByteLength != len(body) ||
				got.Digest != fmt.Sprintf("%x", sha256.Sum256([]byte(body))) || got.Truncated != (len(body) > 16384) ||
				got.Repository != "o/p1" || got.Branch != "feature" || got.PrNumber != 7 ||
				got.HeadSha != o.PRHeadSHA || got.AttemptId != *r.view(r.own, id).Task.CurrentAttemptId ||
				!got.FetchedAt.Equal(o.ObservedAt) {
				t.Fatalf("description = %+v", got)
			}
			if body == "hello" && got.Digest != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
				t.Fatal("incorrect SHA-256")
			}
		})
	}
}

func TestDescriptionUnavailableAndFailedObservationClear(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	for _, tc := range []struct {
		name string
		body *string
		err  error
	}{
		{"null", nil, nil},
		{"nul", ptr("Edited\x00description"), nil},
		{"invalid_utf8", ptr("invalid\xfftext"), nil},
		{"failed_read", ptr("untrusted response"), errors.New("github is down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.obs.set(descriptionObservation(ptr("original")), nil)
			r.project()
			if r.view(r.own, id).Projection.PrDescription == nil {
				t.Fatal("initial description missing")
			}
			r.obs.set(descriptionObservation(tc.body), tc.err)
			r.project()
			if r.view(r.own, id).Projection.PrDescription != nil ||
				r.count(`SELECT count(*) FROM tasks WHERE id = $1 AND pr_description IS NULL`, id) != 1 {
				t.Fatal("unavailable description survived in storage or the read")
			}
			if tc.name != "null" && string(r.view(r.own, id).Projection.PrState) != "unknown" {
				t.Fatal("failed observation retained trusted PR facts")
			}
		})
	}
}

func TestDescriptionSameHeadEditsAndUnchangedRefresh(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	for _, body := range []string{"first", "same-head edit", strings.Repeat("a", 16384) + "suffix A", strings.Repeat("a", 16384) + "suffix B"} {
		before, events := r.view(r.own, id), r.updates(id)
		o := descriptionObservation(&body)
		o.ObservedAt = time.Now().Add(-2 * time.Second).UTC().Truncate(time.Microsecond)
		r.obs.set(o, nil)
		r.project()
		changed := r.view(r.own, id)
		if changed.Task.Version != before.Task.Version+1 || r.updates(id) != events+1 {
			t.Fatal("body edit did not increment version and publish exactly one event")
		}
		if before.Projection.PrDescription != nil && changed.Projection.PrDescription.Digest == before.Projection.PrDescription.Digest {
			t.Fatal("body edit did not update full-body digest")
		}
		// Use a past timestamp so the read remains fresh without waiting for wall time.
		o.ObservedAt = time.Now().Add(-time.Second).UTC().Truncate(time.Microsecond)
		r.obs.set(o, nil)
		r.project()
		refreshed := r.view(r.own, id)
		if refreshed.Task.Version != changed.Task.Version || r.updates(id) != events+1 ||
			refreshed.Projection.PrDescription == nil || !refreshed.Projection.PrDescription.FetchedAt.Equal(o.ObservedAt) ||
			!refreshed.Projection.PrDescription.FetchedAt.After(changed.Projection.PrDescription.FetchedAt) ||
			refreshed.Projection.ObservedAt == nil || !refreshed.Projection.ObservedAt.Equal(o.ObservedAt) {
			t.Fatal("unchanged refresh churned or failed to preserve fetch time")
		}
	}
	o := descriptionObservation(ptr("new head body"))
	o.PRHeadSHA = strings.Repeat("b", 40)
	r.obs.set(o, nil)
	r.project()
	if d := r.view(r.own, id).Projection.PrDescription; d == nil || d.HeadSha != o.PRHeadSHA || d.Excerpt != *o.PRBody {
		t.Fatalf("new-head description = %+v", d)
	}
}

func TestDescriptionReadMasksIdentityAndAge(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	child := r.agent(dbstate.RoleChild, "kid-p")
	for _, tc := range []struct {
		name, update string
	}{
		{"repository", `pr_repository = 'other/repo'`},
		{"number", `pr_number = 8`},
		{"head", `pr_head_sha = repeat('b', 40)`},
		{"branch", `pr_description = jsonb_set(pr_description, '{branch}', '"other"')`},
		{"attempt", `pr_description = jsonb_set(pr_description, '{attempt_id}', '"other-attempt"')`},
		{"unknown_pr", `pr_state = 'unknown'`},
		{"stale", `pr_description = jsonb_set(pr_description, '{fetched_at}', to_jsonb(now() - interval '6 minutes')), projected_at = now()`},
		{"future", `pr_description = jsonb_set(pr_description, '{fetched_at}', to_jsonb(now() + interval '1 minute'))`},
		{"missing_time", `pr_description = pr_description - 'fetched_at'`},
		{"invalid_digest", `pr_description = jsonb_set(pr_description, '{digest}', to_jsonb(repeat('0', 64)))`},
		{"invalid_length", `pr_description = jsonb_set(pr_description, '{byte_length}', '99')`},
		{"invalid_truncated", `pr_description = jsonb_set(pr_description, '{truncated}', 'true')`},
		{"unexpected_metadata", `pr_description = pr_description || '{"review_accepted": true}'::jsonb`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.obs.set(descriptionObservation(ptr("body")), nil)
			r.project()
			if d := r.view(child, id).Projection.PrDescription; d == nil || d.Excerpt != "body" {
				t.Fatal("scoped native read did not expose the fresh description")
			}
			r.exec("UPDATE tasks SET "+tc.update+" WHERE id = $1", id)
			before, calls, events := r.view(r.own, id), r.obs.calls, r.updates(id)
			for _, principal := range []tasks.Principal{r.own, child} {
				got := r.view(principal, id)
				if got.Projection.PrDescription != nil || got.Task.Version != before.Task.Version {
					t.Fatalf("masked read = %+v", got.Projection)
				}
			}
			if r.obs.calls != calls || r.updates(id) != events {
				t.Fatal("DB-only read changed events or called GitHub")
			}
		})
	}
	// A fresh description uses its own timestamp, even if the generic timestamp is stale.
	r.obs.set(descriptionObservation(ptr("body")), nil)
	r.project()
	r.exec(`UPDATE tasks SET projected_at = now() - interval '6 minutes' WHERE id = $1`, id)
	if r.view(child, id).Projection.PrDescription == nil {
		t.Fatal("generic timestamp replaced the description's own fetch age")
	}
}

func TestDescriptionDiscardsChangedTaskDuringCollection(t *testing.T) {
	for _, update := range []string{`current_attempt_id = NULL`, `checkout_branch = 'other'`} {
		t.Run(update, func(t *testing.T) {
			r := newRig(t)
			id := r.projected()
			before, events := r.view(r.own, id), r.updates(id)
			r.obs.set(descriptionObservation(ptr("old task body")), nil)
			r.obs.onObserve = func() { r.exec("UPDATE tasks SET "+update+" WHERE id = $1", id) }
			r.project()
			got := r.view(r.own, id)
			if got.Projection.PrDescription != nil || got.Projection.PrNumber != nil || got.Task.Version != before.Task.Version || r.updates(id) != events {
				t.Fatal("observation crossed a task identity change")
			}
		})
	}
}

func TestDescriptionInconsistentObservationClears(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	for _, change := range []func(*tasks.Observation){
		func(o *tasks.Observation) { o.PRRepo = "o/other" },
		func(o *tasks.Observation) { o.PRHeadRef = "other" },
		func(o *tasks.Observation) { o.PRNumber = 0 },
		func(o *tasks.Observation) { o.PRState = "unknown" },
	} {
		r.obs.set(descriptionObservation(ptr("body")), nil)
		r.project()
		o := descriptionObservation(ptr("inconsistent body"))
		change(&o)
		r.obs.set(o, nil)
		r.project()
		if r.view(r.own, id).Projection.PrDescription != nil ||
			r.count(`SELECT count(*) FROM tasks WHERE id = $1 AND pr_description IS NULL`, id) != 1 {
			t.Fatal("inconsistent observation retained a description")
		}
	}
}
