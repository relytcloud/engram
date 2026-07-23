package memorylake

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// metaReviewAfter stores an explicit override for a fact's next review
// instant, set by MarkReviewed to reset the decay clock. When absent,
// review-due-ness is computed from the fact's created_at plus its type's
// decay offset (see decayReviewAfterMonths) — the same fallback
// internal/store uses when it stamps review_after at insert time from the
// same table (see internal/store/store.go around line 2371). Prefixed
// engram_ like every other key this backend stamps (see mapper.go).
const metaReviewAfter = "engram_review_after"

// decayReviewAfterMonths mirrors internal/store's decayReviewAfterMonths map
// verbatim — the month offset added to a fact's created_at (or to "now" on
// MarkReviewed) to compute its next review_after — see
// internal/store/store.go:250 (decayReviewAfterMonths, built from
// decayDecisionMonths=6, decayPolicyMonths=12, decayPreferenceMonths=3).
// Types absent from this map never need review, mirroring store leaving
// review_after NULL for untracked types. Copied here (not exported by
// internal/store) — keep the numbers in sync if store.go's table ever
// changes.
var decayReviewAfterMonths = map[string]int{
	"decision":   6,
	"policy":     12,
	"preference": 3,
}

// defaultReviewLimit mirrors internal/store's ObservationsNeedingReview
// falling back to s.cfg.MaxContextResults when limit<=0 (store.go:2498).
// This backend has no equivalent config field (see maxFormatContextRecent's
// doc comment for the same accepted limitation), so a fixed reasonable
// default is used instead.
const defaultReviewLimit = 20

// parseFactTime mirrors internal/store's unexported parseObservationTime:
// tries RFC3339(/Nano), the sqlite datetime layout, and a bare date, in that
// order, returning UTC — see internal/store/store.go:parseObservationTime.
// Copied here (not exported by internal/store) so this backend applies the
// exact same review-due comparison rules to MemoryLake's created_at strings
// and to this backend's own locally-generated engram_review_after overrides.
func parseFactTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	formats := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range formats {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

// factReviewAfter computes fact f's next review_after instant. ok is false
// when the fact never needs review: no explicit engram_review_after override
// is stamped (see MarkReviewed) AND its engram_type is absent from
// decayReviewAfterMonths, mirroring store's NULL review_after for untracked
// types.
func factReviewAfter(f Fact) (time.Time, bool) {
	if raw := metaString(f.Metadata, metaReviewAfter); raw != "" {
		if t, err := parseFactTime(raw); err == nil {
			return t, true
		}
	}
	months, ok := decayReviewAfterMonths[metaString(f.Metadata, metaType)]
	if !ok {
		return time.Time{}, false
	}
	created, err := parseFactTime(f.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return created.AddDate(0, months, 0), true
}

// ObservationsNeedingReview lists the project's non-expired facts whose
// computed review_after (see factReviewAfter) has passed, ordered
// soonest-due first — mirroring internal/store's ObservationsNeedingReview
// query shape: deleted/expired excluded, review_after <= now, ORDER BY
// review_after ASC with a stable tie-break (see store.go:2498).
//
// project is accepted for signature compatibility but ignored, the same as
// CountObservationsForProject/FormatContext: a backend instance is already
// bound to a single MemoryLake project.
func (b *MemoryLakeBackend) ObservationsNeedingReview(project string, limit int) ([]store.Observation, error) {
	_ = project
	if limit <= 0 {
		limit = defaultReviewLimit
	}

	facts, err := b.client.listAllFacts(b.ws, b.projID)
	if err != nil {
		return nil, err
	}

	type due struct {
		f  Fact
		at time.Time
	}
	var overdue []due
	now := time.Now().UTC()
	for _, f := range facts {
		if f.Expired {
			continue
		}
		at, ok := factReviewAfter(f)
		if !ok || at.After(now) {
			continue
		}
		overdue = append(overdue, due{f, at})
	}
	sort.SliceStable(overdue, func(i, j int) bool {
		if !overdue[i].at.Equal(overdue[j].at) {
			return overdue[i].at.Before(overdue[j].at)
		}
		return overdue[i].f.ID < overdue[j].f.ID
	})
	if len(overdue) > limit {
		overdue = overdue[:limit]
	}

	out := make([]store.Observation, 0, len(overdue))
	for _, d := range overdue {
		o := ObservationFromFact(d.f)
		o.ID = b.idmap.IntFor(b.projID, d.f.ID)
		o.CreatedAt = d.f.CreatedAt
		o.UpdatedAt = d.f.UpdatedAt
		reviewAfter := d.at.Format("2006-01-02 15:04:05")
		o.ReviewAfter = &reviewAfter
		out = append(out, o)
	}
	return out, nil
}

// MarkReviewed resets the fact's review clock to now plus its type's decay
// offset, mirroring internal/store's MarkReviewed (store.go:2523). The reset
// is stamped as an explicit engram_review_after metadata override so a later
// ObservationsNeedingReview call prefers it over recomputing from
// created_at — created_at never changes, so without an override the next
// review would always compute to the exact same instant as the first one.
// Types absent from decayReviewAfterMonths clear the override entirely,
// mirroring store resetting review_after to NULL for untracked types.
func (b *MemoryLakeBackend) MarkReviewed(syncID string) error {
	id, ok := parseSyncID(syncID)
	if !ok {
		return &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}
	factID, ok := b.factForID(id)
	if !ok {
		return &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}

	current, err := b.getFact(factID)
	if err != nil {
		return err
	}

	md := map[string]any{}
	for k, v := range current.Metadata {
		md[k] = v
	}
	if months, ok := decayReviewAfterMonths[metaString(current.Metadata, metaType)]; ok {
		md[metaReviewAfter] = time.Now().UTC().AddDate(0, months, 0).Format("2006-01-02 15:04:05")
	} else {
		delete(md, metaReviewAfter)
	}

	_, err = b.patchFact(factID, map[string]any{"metadata": md})
	return err
}
