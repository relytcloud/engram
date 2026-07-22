package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// TestSearchFacts_SemanticMapsContentAndRank drives the main path: POST
// .../memories/search returns facts with a score and engram metadata;
// SearchFacts must map each into a store.SearchResult whose Observation
// content comes from metadata["engram_raw"] (not the paraphrased f.Fact) and
// whose Rank equals the fact's score.
func TestSearchFacts_SemanticMapsContentAndRank(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["query"] != "dark mode" {
				t.Errorf("query=%v, want 'dark mode'", body["query"])
			}
			projIDs, _ := body["project_ids"].([]any)
			if len(projIDs) != 1 || projIDs[0] != "proj-1" {
				t.Errorf("project_ids=%v, want [proj-1]", body["project_ids"])
			}
			actorIDs, _ := body["actor_ids"].([]any)
			if len(actorIDs) != 1 || actorIDs[0] != "actor-1" {
				t.Errorf("actor_ids=%v, want [actor-1]", body["actor_ids"])
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"facts": []map[string]any{
						{
							"id":    "fact-1",
							"fact":  "User likes dark mode (paraphrased)",
							"score": 0.87,
							"metadata": map[string]any{
								metaRaw:   "user prefers dark mode, verbatim",
								metaTitle: "dark mode preference",
								metaType:  "preference",
								metaScope: "global",
							},
							"created_at": "2026-07-22T00:00:00Z",
							"updated_at": "2026-07-22T00:00:00Z",
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	results, err := c.SearchFacts("ws-1", "proj-1", "actor-1", "dark mode", store.SearchOptions{})
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Content != "user prefers dark mode, verbatim" {
		t.Fatalf("Content=%q, want engram_raw verbatim text", results[0].Content)
	}
	if results[0].Rank != 0.87 {
		t.Fatalf("Rank=%v, want 0.87 (the fact's score)", results[0].Rank)
	}
}

// TestSearchFacts_FiltersByType verifies client-side filtering: when
// opts.Type is set, only facts whose metadata["engram_type"] matches survive
// — MemoryLake's search endpoint has no server-side type filter for facts, so
// this must happen after decoding the response.
func TestSearchFacts_FiltersByType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"facts": []map[string]any{
					{
						"id":       "fact-1",
						"fact":     "a decision was made",
						"score":    0.9,
						"metadata": map[string]any{metaRaw: "decision content", metaType: "decision"},
					},
					{
						"id":       "fact-2",
						"fact":     "a bug was found",
						"score":    0.8,
						"metadata": map[string]any{metaRaw: "bug content", metaType: "bug"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	results, err := c.SearchFacts("ws-1", "proj-1", "actor-1", "anything", store.SearchOptions{Type: "decision"})
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (only the 'decision' type fact)", len(results))
	}
	if results[0].Content != "decision content" {
		t.Fatalf("Content=%q, want 'decision content'", results[0].Content)
	}
}

// TestSearchFacts_TopicKeySlashPinsFuzzyHits verifies the topic_key fast
// path: when query contains "/", SearchFacts must additionally issue a GET
// .../memories/facts?fact_fuzzy=<query> substring lookup, and any fact it
// hits must be placed ahead of the purely-semantic results, deduplicated by
// fact id against the semantic set.
func TestSearchFacts_TopicKeySlashPinsFuzzyHits(t *testing.T) {
	var fuzzyCalled bool
	var fuzzyTerm string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"facts": []map[string]any{
						{
							"id":       "fact-semantic",
							"fact":     "semantic-only hit",
							"score":    0.95,
							"metadata": map[string]any{metaRaw: "semantic-only content"},
						},
					},
				},
			})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			fuzzyCalled = true
			fuzzyTerm = r.URL.Query().Get("fact_fuzzy")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items": []map[string]any{
						{
							"id":       "fact-topic",
							"fact":     "topic key hit",
							"metadata": map[string]any{metaRaw: "topic key content", metaTopicKey: "architecture/auth-model"},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	results, err := c.SearchFacts("ws-1", "proj-1", "actor-1", "architecture/auth-model", store.SearchOptions{})
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if !fuzzyCalled {
		t.Fatal("expected a fact_fuzzy GET request, got none")
	}
	if fuzzyTerm != "architecture/auth-model" {
		t.Fatalf("fact_fuzzy=%q, want 'architecture/auth-model'", fuzzyTerm)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (topic hit + semantic hit)", len(results))
	}
	if results[0].Content != "topic key content" {
		t.Fatalf("results[0].Content=%q, want the fact_fuzzy hit pinned first", results[0].Content)
	}
	if results[1].Content != "semantic-only content" {
		t.Fatalf("results[1].Content=%q, want the semantic hit second", results[1].Content)
	}
}

// TestSearchFacts_FuzzyFailureDegradesToSemanticOnly is the FIX #7 regression:
// the fact_fuzzy lookup is only a fast-path that pins topic_key hits ahead of
// the semantic results. When it fails, SearchFacts must NOT discard the
// already-successful semantic results by returning an error — it degrades to
// semantic-only (logging to stderr) and returns them.
func TestSearchFacts_FuzzyFailureDegradesToSemanticOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"facts": []map[string]any{
						{"id": "fact-semantic", "fact": "semantic hit", "score": 0.9, "metadata": map[string]any{metaRaw: "semantic content"}},
					},
				},
			})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			// Fuzzy lookup fails hard.
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]any{"code": "BOOM", "message": "fuzzy exploded"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	// query contains "/" so the fuzzy fast-path is attempted (and fails).
	results, err := c.SearchFacts("ws-1", "proj-1", "actor-1", "architecture/auth-model", store.SearchOptions{})
	if err != nil {
		t.Fatalf("SearchFacts: %v, want nil (fuzzy failure must degrade to semantic-only, not error)", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (the semantic hit must survive the fuzzy failure)", len(results))
	}
	if results[0].Content != "semantic content" {
		t.Fatalf("Content=%q, want 'semantic content'", results[0].Content)
	}
}

// TestSearchFacts_DedupesOverlapBetweenFuzzyAndSemantic verifies that when
// the same fact id appears in both the fact_fuzzy substring hits and the
// semantic search results, it is kept exactly once — pinned at its fuzzy
// position, not duplicated.
func TestSearchFacts_DedupesOverlapBetweenFuzzyAndSemantic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"facts": []map[string]any{
						{"id": "fact-shared", "fact": "shared", "score": 0.5, "metadata": map[string]any{metaRaw: "shared content"}},
					},
				},
			})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items": []map[string]any{
						{"id": "fact-shared", "fact": "shared", "metadata": map[string]any{metaRaw: "shared content"}},
					},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	results, err := c.SearchFacts("ws-1", "proj-1", "actor-1", "architecture/auth-model", store.SearchOptions{})
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (deduped by fact id)", len(results))
	}
}
