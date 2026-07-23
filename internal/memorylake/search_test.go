package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// TestSearchFacts_SemanticMapsContentAndRank drives the main path: POST
// .../memories/search returns facts with a score and engram metadata;
// SearchFacts must map each into a store.SearchResult whose Observation
// content is the fact's own text (mem0's text — see mapper.go's doc comment
// on why engram_raw is no longer read) and whose Rank equals the fact's score.
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
							"fact":  "User likes dark mode",
							"score": 0.87,
							"metadata": map[string]any{
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
	if results[0].Content != "User likes dark mode" {
		t.Fatalf("Content=%q, want the fact's own text", results[0].Content)
	}
	if results[0].SyncID != "fact-1" {
		t.Fatalf("SyncID=%q, want fact-1", results[0].SyncID)
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
						"metadata": map[string]any{metaType: "decision"},
					},
					{
						"id":       "fact-2",
						"fact":     "a bug was found",
						"score":    0.8,
						"metadata": map[string]any{metaType: "bug"},
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
	if results[0].Content != "a decision was made" {
		t.Fatalf("Content=%q, want 'a decision was made'", results[0].Content)
	}
}

// TestSearchFacts_SlashQueryDoesNotTriggerFactFuzzy is the Task 9 regression:
// retrieval is now purely semantic. A query containing "/" (the shape of an
// engram topic_key, e.g. "architecture/auth-model") used to additionally
// trigger a GET .../memories/facts?fact_fuzzy=<query> substring lookup; that
// fast-path is retired, so such a query must issue only the semantic POST
// and nothing else.
func TestSearchFacts_SlashQueryDoesNotTriggerFactFuzzy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["query"] != "architecture/auth-model" {
				t.Errorf("query=%v, want 'architecture/auth-model'", body["query"])
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"facts": []map[string]any{
						{
							"id":       "fact-topic",
							"fact":     "topic key hit",
							"score":    0.9,
							"metadata": map[string]any{metaTopicKey: "architecture/auth-model"},
						},
					},
				},
			})
		case strings.Contains(r.URL.RawQuery, "fact_fuzzy"):
			t.Fatalf("unexpected fact_fuzzy request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
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
		t.Fatalf("got %d results, want 1 (semantic hit only)", len(results))
	}
	if results[0].Content != "topic key hit" {
		t.Fatalf("Content=%q, want 'topic key hit'", results[0].Content)
	}
	if results[0].SyncID != "fact-topic" {
		t.Fatalf("SyncID=%q, want fact-topic", results[0].SyncID)
	}
}

// TestSearchFacts_MissingTypeScopeMetadataPassesFilter verifies that under
// the Option A thin adapter (engram no longer stamps engram_type/engram_scope
// onto a fact at save time), a fact whose metadata lacks those keys is not
// filtered out just because a caller passed opts.Type/opts.Scope — absence of
// the metadata is treated as "undecided", not "mismatch".
func TestSearchFacts_MissingTypeScopeMetadataPassesFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"facts": []map[string]any{
					{"id": "fact-1", "fact": "no metadata at all", "score": 0.7, "metadata": map[string]any{}},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	results, err := c.SearchFacts("ws-1", "proj-1", "actor-1", "anything", store.SearchOptions{Type: "decision", Scope: "global"})
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (missing metadata must not be filtered out)", len(results))
	}
}
