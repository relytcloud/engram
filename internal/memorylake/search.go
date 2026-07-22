package memorylake

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

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
// Two MemoryLake requests can contribute to the result:
//
//  1. Semantic search — POST .../workspaces/{ws}/memories/search — always
//     issued. This is the primary path: MemoryLake ranks facts by
//     similarity to query and returns each with a score.
//  2. fact_fuzzy substring lookup — GET
//     .../workspaces/{ws}/projects/{proj}/memories/facts?fact_fuzzy=<query>
//     — issued only when query contains "/", which is the shape of an
//     engram topic_key (e.g. "architecture/auth-model"). A topic_key is an
//     exact identifier, not a prose query, so semantic similarity search is
//     the wrong tool for it; a substring match against MemoryLake's stored
//     facts (which carry the topic_key in metadata) finds it reliably. Hits
//     from this lookup are pinned ahead of the semantic results.
//
// Both sets are mapped through ObservationFromFact (so Content prefers the
// verbatim engram_raw metadata over MemoryLake's own paraphrased fact text),
// filtered client-side against opts.Type / opts.Scope (compared against the
// engram_type / engram_scope metadata keys — MemoryLake's search API has no
// server-side equivalent), deduplicated by fact id (a fact_fuzzy hit wins
// over a semantic hit for the same id, since it is placed first), and
// returned in the order: pinned fact_fuzzy hits first, then semantic hits by
// score descending.
//
// opts.Project is intentionally not used as a client-side filter here:
// projID (the MemoryLake project id, already resolved by the caller) is what
// actually scopes both requests, and MemoryLake facts don't carry engram's
// human-readable project name in metadata to compare against. opts.MatchMode
// (all/any term matching) is an FTS5 concept with no MemoryLake analogue and
// is likewise not applicable here — MemoryLake's semantic search takes the
// query as free text.
func (c *Client) SearchFacts(ws, projID, actorID, query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	semanticFacts, err := c.semanticSearchFacts(ws, projID, actorID, query, opts)
	if err != nil {
		return nil, err
	}

	var pinnedFacts []Fact
	if strings.Contains(query, "/") {
		pinnedFacts, err = c.fuzzyFacts(ws, projID, query)
		if err != nil {
			// The fuzzy lookup is only a fast-path that pins topic_key hits
			// ahead of the semantic results; the semantic search above has
			// already succeeded. Discarding those results just because the
			// (best-effort) fuzzy request failed would turn a partial-quality
			// search into a hard error. Log and degrade to semantic-only.
			fmt.Fprintf(os.Stderr, "[engram] memorylake: fuzzy fact lookup for %q failed, using semantic results only: %v\n", query, err)
			pinnedFacts = nil
		}
	}

	seen := make(map[string]bool, len(pinnedFacts)+len(semanticFacts))
	results := make([]store.SearchResult, 0, len(pinnedFacts)+len(semanticFacts))

	appendFact := func(f Fact, rank float64) {
		if seen[f.ID] {
			return
		}
		if !factMatchesFilter(f, opts) {
			return
		}
		seen[f.ID] = true
		results = append(results, store.SearchResult{
			Observation: ObservationFromFact(f),
			Rank:        rank,
		})
	}

	for _, f := range pinnedFacts {
		appendFact(f, f.Score)
	}
	for _, f := range semanticFacts {
		appendFact(f, f.Score)
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

// fuzzyFacts issues the GET .../projects/{proj}/memories/facts?fact_fuzzy=
// substring lookup used to pin topic_key hits ahead of semantic results.
//
// Deliberately single-page (page_size=200, no continuation_token
// following), unlike listAllFacts/listAllProjects/etc: a topic_key is a
// specific slash-separated identifier (see engram's topic_key format), so a
// substring match against it is expected to hit a handful of facts at most,
// never the whole project. A project needing >200 fact_fuzzy hits for one
// topic_key is already outside what this pinning fast-path is designed for
// (see spec §11.5) — SearchFacts still returns full, correct results in that
// case via the semantic results it merges these into, just without the
// pinning boost past the first page.
func (c *Client) fuzzyFacts(ws, projID, term string) ([]Fact, error) {
	var out struct {
		Items []Fact `json:"items"`
	}
	path := "/api/v3/workspaces/" + ws + "/projects/" + projID +
		"/memories/facts?fact_fuzzy=" + url.QueryEscape(term) + "&page_size=200"
	if err := c.doJSON("GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// factMatchesFilter applies the client-side opts.Type / opts.Scope filter
// against a fact's engram metadata. An empty opts field means "no filter" for
// that dimension.
func factMatchesFilter(f Fact, opts store.SearchOptions) bool {
	get := func(k string) string {
		if v, ok := f.Metadata[k].(string); ok {
			return v
		}
		return ""
	}
	if opts.Type != "" && get(metaType) != opts.Type {
		return false
	}
	if opts.Scope != "" && get(metaScope) != opts.Scope {
		return false
	}
	return true
}
