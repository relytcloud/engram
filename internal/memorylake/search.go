package memorylake

import (
	"sort"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// defaultSearchTopK / defaultSearchThreshold are used when opts.Limit is
// unset (<=0). MemoryLake's search endpoint requires a top_k and accepts a
// similarity threshold; neither has a meaningful zero value, so SearchFacts
// falls back to modest, engram-appropriate defaults rather than omitting
// them.
const (
	defaultSearchTopK      = 10
	defaultSearchThreshold = 0.1
)

// SearchFacts is the read-path counterpart to AppendObservation/BackfillFacts:
// it queries MemoryLake for facts matching query within workspace ws and
// project projID, and maps whatever it gets back into store.SearchResult so
// callers can treat a MemoryLake-backed project the same way they treat the
// local SQLite+FTS5 store.
//
// Retrieval is purely semantic: POST .../workspaces/{ws}/memories/search,
// ranked by MemoryLake's own similarity score. Earlier builds additionally
// issued a fact_fuzzy substring lookup (GET
// .../projects/{proj}/memories/facts?fact_fuzzy=<query>) whenever query
// contained "/", on the theory that an engram topic_key (e.g.
// "architecture/auth-model") is an exact identifier better served by
// substring match than semantic similarity, and pinned those hits ahead of
// the semantic results. That fast-path is retired: semantic search is the
// only retrieval path now (see spec §2/§4) — there is no fuzzy augmentation
// and no pinning/dedup merge step to reason about.
//
// Results are mapped through ObservationFromFact (Content is fact.Fact,
// SyncID is fact.ID, Rank is fact.Score), then filtered client-side against
// opts.Type / opts.Scope (compared against the engram_type / engram_scope
// metadata keys — MemoryLake's search API has no server-side equivalent).
// Under the Option A thin adapter (spec §2/§4), engram no longer writes
// engram_type/engram_scope onto a fact at save time (fact structure and
// content are mem0's to own) — so on facts created since that change, this
// metadata is normally absent. See factMatchesFilter for how that absence is
// handled: it is not treated as "doesn't match", so the filter degrades to a
// no-op (returns everything) rather than silently discarding results that
// simply predate/lack the metadata.
//
// opts.Project is intentionally not used as a client-side filter here:
// projID (the MemoryLake project id, already resolved by the caller) is what
// actually scopes the request, and MemoryLake facts don't carry engram's
// human-readable project name in metadata to compare against. opts.MatchMode
// (all/any term matching) is an FTS5 concept with no MemoryLake analogue and
// is likewise not applicable here — MemoryLake's semantic search takes the
// query as free text.
func (c *Client) SearchFacts(ws, projID, actorID, query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	semanticFacts, err := c.semanticSearchFacts(ws, projID, actorID, query, opts)
	if err != nil {
		return nil, err
	}

	results := make([]store.SearchResult, 0, len(semanticFacts))
	for _, f := range semanticFacts {
		if !factMatchesFilter(f, opts) {
			continue
		}
		results = append(results, store.SearchResult{
			Observation: ObservationFromFact(f),
			Rank:        f.Score,
		})
	}

	return results, nil
}

// semanticSearchFacts issues the POST .../memories/search request and
// returns the raw facts MemoryLake ranked, sorted by score descending (the
// server is expected to already return them in that order, but sorting
// explicitly means SearchFacts doesn't depend on that being true).
func (c *Client) semanticSearchFacts(ws, projID, actorID, query string, opts store.SearchOptions) ([]Fact, error) {
	topK := opts.Limit
	if topK <= 0 {
		topK = defaultSearchTopK
	}

	body := map[string]any{
		"query":        query,
		"project_ids":  []string{projID},
		"memory_types": []string{"fact"},
		"top_k":        topK,
		"threshold":    defaultSearchThreshold,
	}
	if actorID != "" {
		body["actor_ids"] = []string{actorID}
	}

	var out struct {
		Facts []Fact `json:"facts"`
	}
	path := "/api/v3/workspaces/" + ws + "/memories/search"
	if err := c.doJSON("POST", path, body, &out); err != nil {
		return nil, err
	}

	sort.SliceStable(out.Facts, func(i, j int) bool {
		return out.Facts[i].Score > out.Facts[j].Score
	})
	return out.Facts, nil
}

// factMatchesFilter applies the client-side opts.Type / opts.Scope filter
// against a fact's engram metadata. An empty opts field means "no filter" for
// that dimension.
//
// A fact whose metadata simply lacks the engram_type/engram_scope key is
// treated as passing the filter for that dimension, not as failing it: under
// the Option A thin adapter, engram no longer stamps that metadata at save
// time (see the SearchFacts doc comment), so most facts have neither key.
// Rejecting them whenever a caller passes opts.Type/opts.Scope would turn the
// filter into a near-total blackout instead of the best-effort narrowing it
// is meant to be; the honest behavior for metadata engram no longer controls
// is to let it through undecided rather than pretend a mismatch was
// observed.
func factMatchesFilter(f Fact, opts store.SearchOptions) bool {
	get := func(k string) (string, bool) {
		v, ok := f.Metadata[k].(string)
		return v, ok
	}
	if opts.Type != "" {
		if v, ok := get(metaType); ok && v != opts.Type {
			return false
		}
	}
	if opts.Scope != "" {
		if v, ok := get(metaScope); ok && v != opts.Scope {
			return false
		}
	}
	return true
}
