package memorylake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// hashNormalizedContent mirrors internal/store's unexported hashNormalized:
// lowercase + collapse whitespace runs to single spaces + full sha256 hex
// digest — see internal/store/store.go:6516 (hashNormalized). Copied here
// (not exported by internal/store) so PassiveCapture's dedup matches the
// same normalization the local store applies before comparing learnings by
// content hash. Keep in sync if that rule ever changes.
func hashNormalizedContent(content string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(content), " "))
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:])
}

// passiveDedupKey builds the local dedup key PassiveCapture uses to detect a
// learning it has already saved for project: normalized project + the
// content's normalized hash. Prefixed "passive:" so it can never collide
// with a MemoryLake fact id, provisional message id, or a prompt dedup key
// sharing the same IDMap key space (see idmap.go's IntIfExists doc comment).
func passiveDedupKey(project, learning string) string {
	norm, _ := store.NormalizeProject(project)
	return "passive:" + norm + ":" + hashNormalizedContent(learning)
}

// PassiveCapture mirrors internal/store's PassiveCapture (store.go:6689):
// extract learnings from p.Content via the shared store.ExtractLearnings
// (parses "## Key Learnings"-style sections), skip any learning whose
// normalized content hash was already saved for this project (see
// passiveDedupKey), and save the rest via AddObservation with the same
// type="passive", 60-char-truncated title, and scope="project" convention
// store.go's PassiveCapture uses.
//
// Unlike the local store, which checks the dedup hash against a live SQL
// column (`observations.normalized_hash`) that also excludes soft-deleted
// rows, this backend's dedup set (borrowed from the shared per-project
// IDMap, see idmap.go's IntIfExists doc comment) only ever grows: once a
// learning's hash has been recorded here it is never un-recorded even if the
// resulting observation is later deleted via DeleteObservation. This is an
// accepted, documented divergence — reproducing store's live exclusion would
// require either a second listAllFacts scan per learning (defeating the
// point of a local cache) or teaching DeleteObservation about this dedup
// namespace, which is out of this task's scope.
func (b *MemoryLakeBackend) PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error) {
	result := &store.PassiveCaptureResult{}

	learnings := store.ExtractLearnings(p.Content)
	result.Extracted = len(learnings)
	if len(learnings) == 0 {
		return result, nil
	}

	for _, learning := range learnings {
		key := passiveDedupKey(p.Project, learning)
		if _, ok := b.idmap.IntIfExists(key); ok {
			result.Duplicates++
			continue
		}

		title := learning
		if len(title) > 60 {
			title = title[:60] + "..."
		}

		if _, err := b.AddObservation(store.AddObservationParams{
			SessionID: p.SessionID,
			Type:      "passive",
			Title:     title,
			Content:   learning,
			Project:   p.Project,
			Scope:     "project",
			ToolName:  p.Source,
		}); err != nil {
			return result, fmt.Errorf("passive capture save: %w", err)
		}
		// Record as seen so a later identical learning (same normalized
		// content, same project) dedupes instead of saving a second fact.
		b.idmap.IntFor(key)
		result.Saved++
	}

	return result, nil
}
