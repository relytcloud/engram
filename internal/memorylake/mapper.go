package memorylake

import "github.com/Gentleman-Programming/engram/internal/store"

// Fact mirrors the shape of a MemoryLake "fact" record as returned by its
// extraction/search APIs. It is the wire format engram translates
// store.Observation to and from when the MemoryLake backend is active for a
// project.
type Fact struct {
	ID        string         `json:"id"`
	Fact      string         `json:"fact"`
	Metadata  map[string]any `json:"metadata"`
	Score     float64        `json:"score"`
	Expired   bool           `json:"expired"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// Metadata keys engram still reads/writes on a MemoryLake fact under the
// Option A thin adapter (see docs/superpowers/specs/2026-07-23-memorylake-
// thin-adapter-design.md §2/§4): everything mem0 already owns (content,
// dedup, topic_key upsert, verbatim-original preservation via engram_raw) has
// been dropped — content and structure are mem0's to manage now. What
// remains are the handful of fields Engram itself still surfaces that mem0
// has no concept of: pinned, an explicit review-decay override, and (when a
// caller does an explicit mem_update) title/type/scope/topic_key.
const (
	metaTitle    = "engram_title"
	metaType     = "engram_type"
	metaScope    = "engram_scope"
	metaTopicKey = "topic_key"
)

// ObservationFromFact decodes a MemoryLake Fact back into a store.Observation.
//
// SyncID is the fact's own id — under Option A′ (spec §3) sync_id for a
// MemoryLake-backed observation simply *is* the MemoryLake fact id; there is
// no separate id-mapping layer to resolve it through. ID (the legacy int64
// field) is intentionally left at its zero value: it has no MemoryLake
// analogue any more (that was the retired IDMap's job) and sync_id is the
// handle every mem_* by-id call now takes.
//
// Content is fact.Fact verbatim — mem0's own (possibly LLM-extracted or
// merged) text. Earlier builds preferred a locally-stamped "engram_raw"
// metadata copy of the original save text; that verbatim-preservation path is
// retired along with the rest of the sync write path (see AddObservation):
// engram_raw is no longer written, and is no longer read here even if an
// older fact still carries one.
func ObservationFromFact(f Fact) store.Observation {
	get := func(k string) string {
		if v, ok := f.Metadata[k].(string); ok {
			return v
		}
		return ""
	}

	obs := store.Observation{
		SyncID:        f.ID,
		Content:       f.Fact,
		Title:         get(metaTitle),
		Type:          get(metaType),
		Scope:         get(metaScope),
		RevisionCount: revisionFromMetadata(f.Metadata),
	}
	if tk := get(metaTopicKey); tk != "" {
		obs.TopicKey = &tk
	}
	return obs
}

// revisionFromMetadata decodes metadata[metaRev] into an int, defaulting to 1
// (matching internal/store's revision_count default for a freshly-created
// observation) when the key is absent — e.g. for facts that predate
// engram_rev being stamped (a field only ever written by the now-retired
// topic_key upsert path), or facts created outside engram entirely. JSON
// numbers decode as float64 through map[string]any, so that is the primary
// case handled; int/int64 are also accepted for metadata built in-process
// before ever round-tripping through JSON.
func revisionFromMetadata(md map[string]any) int {
	switch v := md[metaRev].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 1
	}
}

// metaRev mirrors store.Observation.RevisionCount. No code path in this
// package writes it any more (the topic_key upsert PATCH that used to bump it
// is retired along with AddObservation's sync backfill — see backend.go), but
// revisionFromMetadata still reads it so a fact from an older build (or one
// some other system stamped) decodes its revision count instead of silently
// resetting to 1.
const metaRev = "engram_rev"
