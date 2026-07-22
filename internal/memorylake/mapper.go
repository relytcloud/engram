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

// Metadata keys engram stashes on every MemoryLake fact so the original
// Observation can be reconstructed losslessly (or as close to it as
// MemoryLake's extraction allows).
const (
	metaRaw      = "engram_raw"
	metaTitle    = "engram_title"
	metaType     = "engram_type"
	metaScope    = "engram_scope"
	metaObsID    = "engram_obs_id"
	metaTopicKey = "topic_key"
	// metaRev mirrors store.Observation.RevisionCount: incremented each time a
	// topic_key upsert PATCHes an existing fact in place (see backend.go's
	// upsertTopicKeyFact). Set to 1 on first write, matching the local store's
	// revision_count default.
	metaRev = "engram_rev"
)

// FactMetadata builds the metadata map engram attaches to a MemoryLake fact
// when it saves an observation there. raw is the verbatim observation
// content (p.Content, or whatever the caller wants preserved character for
// character) — it is what ObservationFromFact prefers when decoding, since
// MemoryLake's own "fact" field is an LLM-extracted paraphrase and may not
// match the original text.
func FactMetadata(p store.AddObservationParams, obsID string, raw string) map[string]any {
	md := map[string]any{
		metaRaw:   raw,
		metaTitle: p.Title,
		metaType:  p.Type,
		metaScope: p.Scope,
		metaObsID: obsID,
		metaRev:   1,
	}
	if p.TopicKey != "" {
		md[metaTopicKey] = p.TopicKey
	}
	return md
}

// ObservationFromFact decodes a MemoryLake Fact back into a store.Observation.
// Content is taken from metadata["engram_raw"] (the verbatim original text
// engram wrote) when present, falling back to f.Fact (MemoryLake's extracted
// paraphrase) only when no raw copy was recorded — e.g. facts created outside
// engram. ID and the created/updated timestamps are intentionally left zero
// here: the backend layer is responsible for resolving f.ID through an IDMap
// and parsing the MemoryLake timestamps into engram's Observation.CreatedAt /
// UpdatedAt formats.
func ObservationFromFact(f Fact) store.Observation {
	get := func(k string) string {
		if v, ok := f.Metadata[k].(string); ok {
			return v
		}
		return ""
	}

	content := get(metaRaw)
	if content == "" {
		content = f.Fact
	}

	obs := store.Observation{
		Content:       content,
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
// engram_rev being stamped, or facts created outside engram entirely. JSON
// numbers decode as float64 through map[string]any, so that is the primary
// case handled; int/int64 are also accepted for metadata built in-process
// (e.g. FactMetadata's literal 1) before ever round-tripping through JSON.
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
