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
		Content: content,
		Title:   get(metaTitle),
		Type:    get(metaType),
		Scope:   get(metaScope),
	}
	if tk := get(metaTopicKey); tk != "" {
		obs.TopicKey = &tk
	}
	return obs
}
