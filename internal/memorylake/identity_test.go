package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestEnsureProject drives the httptest mock described in the task-5 brief:
// first call sees an empty project list and must POST to create; the second
// call must find the now-existing project by custom_id and NOT POST again
// (idempotency, verified via a request counter).
func TestEnsureProject(t *testing.T) {
	type project struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		CustomID string `json:"custom_id"`
	}
	var projects []project
	var postCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"items": projects},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			atomic.AddInt32(&postCount, 1)
			var body struct {
				CustomID string `json:"custom_id"`
				Name     string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			p := project{ID: "proj-new", Name: body.Name, CustomID: body.CustomID}
			projects = append(projects, p)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": p.ID},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	id, err := c.EnsureProject("ws-1", "myproj")
	if err != nil {
		t.Fatalf("first EnsureProject: %v", err)
	}
	if id != "proj-new" {
		t.Fatalf("first EnsureProject id=%q, want proj-new", id)
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("postCount after first call=%d, want 1", got)
	}

	id2, err := c.EnsureProject("ws-1", "myproj")
	if err != nil {
		t.Fatalf("second EnsureProject: %v", err)
	}
	if id2 != "proj-new" {
		t.Fatalf("second EnsureProject id=%q, want proj-new", id2)
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("postCount after second call=%d, want 1 (must not re-POST)", got)
	}
}

func TestResolveWorkspaceID_PrefixShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no HTTP call expected for ws- prefixed id, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	id, err := c.ResolveWorkspaceID("ws-already-an-id")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID: %v", err)
	}
	if id != "ws-already-an-id" {
		t.Fatalf("id=%q, want passthrough", id)
	}
}

func TestResolveWorkspaceID_MatchesByCustomIDOrName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v3/workspaces" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items": []map[string]any{
					{"id": "ws-1", "name": "Engram", "custom_id": "engram"},
					{"id": "ws-2", "name": "Other", "custom_id": "other"},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	id, err := c.ResolveWorkspaceID("engram")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID by custom_id: %v", err)
	}
	if id != "ws-1" {
		t.Fatalf("id=%q, want ws-1", id)
	}

	id, err = c.ResolveWorkspaceID("Other")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID by name: %v", err)
	}
	if id != "ws-2" {
		t.Fatalf("id=%q, want ws-2", id)
	}
}

func TestResolveWorkspaceID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"items": []map[string]any{}},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	_, err := c.ResolveWorkspaceID("nope")
	if err == nil {
		t.Fatal("expected error for unknown workspace")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Code != "NOT_FOUND" {
		t.Fatalf("code=%q, want NOT_FOUND", apiErr.Code)
	}
}

// TestEnsureActor covers the create-then-bind happy path: POST /actors
// creates the actor, then POST /workspaces/{ws}/actors binds it.
func TestEnsureActor(t *testing.T) {
	var actorPosts, bindPosts int32
	var boundActorID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			atomic.AddInt32(&actorPosts, 1)
			var body struct {
				CustomID    string `json:"custom_id"`
				ActorType   string `json:"actor_type"`
				DisplayName string `json:"display_name"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.ActorType != "HUMAN" {
				t.Fatalf("actor_type=%q, want HUMAN", body.ActorType)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "actor-1"},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			atomic.AddInt32(&bindPosts, 1)
			var body struct {
				ActorID string `json:"actor_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			boundActorID = body.ActorID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	id, err := c.EnsureActor("ws-1", "machine-42", "Philip's Laptop")
	if err != nil {
		t.Fatalf("EnsureActor: %v", err)
	}
	if id != "actor-1" {
		t.Fatalf("id=%q, want actor-1", id)
	}
	if atomic.LoadInt32(&actorPosts) != 1 {
		t.Fatalf("actorPosts=%d, want 1", actorPosts)
	}
	if atomic.LoadInt32(&bindPosts) != 1 {
		t.Fatalf("bindPosts=%d, want 1", bindPosts)
	}
	if boundActorID != "actor-1" {
		t.Fatalf("boundActorID=%q, want actor-1", boundActorID)
	}
}

// TestEnsureActor_BindErrorPropagates ensures a failure binding the actor to
// the workspace is surfaced to the caller rather than silently swallowed.
func TestEnsureActor_BindErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "actor-1"},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "boom", "error_code": "INTERNAL",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	_, err := c.EnsureActor("ws-1", "machine-42", "Philip's Laptop")
	if err == nil {
		t.Fatal("expected bind error to propagate")
	}
}

// ─── Task 16: full pagination ───────────────────────────────────────────────

// TestResolveWorkspaceID_PaginatesAcrossPages drives a two-page GET
// /api/v3/workspaces response (continuation_token) where the target
// workspace only appears on the second page — ResolveWorkspaceID must follow
// the token rather than giving up after page one.
func TestResolveWorkspaceID_PaginatesAcrossPages(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v3/workspaces" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		n := atomic.AddInt32(&gets, 1)
		if r.URL.Query().Get("continuation_token") == "" {
			if n != 1 {
				t.Fatalf("first call should have no continuation_token, got call #%d", n)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items":              []map[string]any{{"id": "ws-1", "name": "Other", "custom_id": "other"}},
					"continuation_token": "page2",
				},
			})
			return
		}
		if r.URL.Query().Get("continuation_token") != "page2" {
			t.Fatalf("unexpected continuation_token=%q", r.URL.Query().Get("continuation_token"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items":              []map[string]any{{"id": "ws-2", "name": "Engram", "custom_id": "engram"}},
				"continuation_token": "",
			},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	id, err := c.ResolveWorkspaceID("engram")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID: %v", err)
	}
	if id != "ws-2" {
		t.Fatalf("id=%q, want ws-2 (found only on page 2)", id)
	}
	if got := atomic.LoadInt32(&gets); got != 2 {
		t.Fatalf("GET count=%d, want exactly 2 (must follow continuation_token)", got)
	}
}

// TestListAllWorkspaces_TerminatesAgainstInfiniteContinuationToken mirrors
// the task-12 listAllFacts hardening test: a server that never stops
// returning a continuation_token must not hang ResolveWorkspaceID, and the
// loop must stop at exactly maxPaginationPages.
func TestListAllWorkspaces_TerminatesAgainstInfiniteContinuationToken(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items":              []map[string]any{{"id": "ws-x", "name": "x", "custom_id": "x"}},
				"continuation_token": "always-more",
			},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	done := make(chan struct{})
	var items []wsItem
	var err error
	go func() {
		items, err = c.listAllWorkspaces()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("listAllWorkspaces did not terminate against an infinite continuation_token")
	}
	if err != nil {
		t.Fatalf("listAllWorkspaces: %v", err)
	}
	if got := atomic.LoadInt32(&gets); got != int32(maxPaginationPages) {
		t.Fatalf("GET count=%d, want exactly maxPaginationPages=%d", got, maxPaginationPages)
	}
	if len(items) != maxPaginationPages {
		t.Fatalf("items=%d, want %d (one per page, capped)", len(items), maxPaginationPages)
	}
}

// TestEnsureProject_PaginatesAcrossPages mirrors
// TestResolveWorkspaceID_PaginatesAcrossPages for the project list: the
// target project only appears on page 2, so EnsureProject must follow
// continuation_token and, having found it, must NOT re-POST to create it.
func TestEnsureProject_PaginatesAcrossPages(t *testing.T) {
	var gets, posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			n := atomic.AddInt32(&gets, 1)
			if r.URL.Query().Get("continuation_token") == "" {
				if n != 1 {
					t.Fatalf("first GET should have no continuation_token, got call #%d", n)
				}
				json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"items":              []map[string]any{{"id": "p-1", "name": "other", "custom_id": "other"}},
						"continuation_token": "page2",
					},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items":              []map[string]any{{"id": "p-2", "name": "myproj", "custom_id": "myproj"}},
					"continuation_token": "",
				},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			atomic.AddInt32(&posts, 1)
			t.Fatal("must not POST when the project is found via pagination")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	id, err := c.EnsureProject("ws-1", "myproj")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if id != "p-2" {
		t.Fatalf("id=%q, want p-2 (found only on page 2)", id)
	}
	if got := atomic.LoadInt32(&gets); got != 2 {
		t.Fatalf("GET count=%d, want exactly 2", got)
	}
	if got := atomic.LoadInt32(&posts); got != 0 {
		t.Fatalf("POST count=%d, want 0", got)
	}
}

// ─── Task 16: EnsureActor dedupe via CUSTOM_ID_CONFLICT ────────────────────

// TestEnsureActor_CustomIDConflict_RecoversExistingActorID drives the
// dedupe path from the task-16 brief: POST /api/v3/actors reports the
// custom_id already exists (error_code CUSTOM_ID_CONFLICT); EnsureActor must
// recover by paging through GET /api/v3/actors to find the actor already
// carrying that custom_id (here, only on page 2), then bind that recovered
// id to the workspace exactly as it would for a freshly created actor —
// never creating a second, orphaned actor for the same identity.
func TestEnsureActor_CustomIDConflict_RecoversExistingActorID(t *testing.T) {
	var actorPosts, actorGets, bindPosts int32
	var boundActorID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			atomic.AddInt32(&actorPosts, 1)
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "custom_id already exists", "error_code": "CUSTOM_ID_CONFLICT",
			})
		case r.Method == "GET" && r.URL.Path == "/api/v3/actors":
			n := atomic.AddInt32(&actorGets, 1)
			if r.URL.Query().Get("continuation_token") == "" {
				if n != 1 {
					t.Fatalf("first GET should have no continuation_token, got call #%d", n)
				}
				json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"items":              []map[string]any{{"id": "actor-other", "custom_id": "someone-else"}},
						"continuation_token": "page2",
					},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items":              []map[string]any{{"id": "actor-existing", "custom_id": "machine-42"}},
					"continuation_token": "",
				},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			atomic.AddInt32(&bindPosts, 1)
			var body struct {
				ActorID string `json:"actor_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			boundActorID = body.ActorID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	id, err := c.EnsureActor("ws-1", "machine-42", "Philip's Laptop")
	if err != nil {
		t.Fatalf("EnsureActor: %v", err)
	}
	if id != "actor-existing" {
		t.Fatalf("id=%q, want actor-existing (recovered via GET /actors)", id)
	}
	if got := atomic.LoadInt32(&actorPosts); got != 1 {
		t.Fatalf("actorPosts=%d, want 1 (the conflicting create attempt)", got)
	}
	if got := atomic.LoadInt32(&actorGets); got != 2 {
		t.Fatalf("actorGets=%d, want 2 (target only on page 2)", got)
	}
	if got := atomic.LoadInt32(&bindPosts); got != 1 {
		t.Fatalf("bindPosts=%d, want 1", got)
	}
	if boundActorID != "actor-existing" {
		t.Fatalf("boundActorID=%q, want actor-existing", boundActorID)
	}
}

// TestEnsureActor_CustomIDConflict_NotFoundInActorList_ReturnsOriginalError
// covers the (unexpected but possible) case where POST reports a conflict
// yet the actor list doesn't contain a matching custom_id — EnsureActor must
// surface the original CUSTOM_ID_CONFLICT rather than inventing a different
// error or silently binding a wrong/empty actor id.
func TestEnsureActor_CustomIDConflict_NotFoundInActorList_ReturnsOriginalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "custom_id already exists", "error_code": "CUSTOM_ID_CONFLICT",
			})
		case r.Method == "GET" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items":              []map[string]any{{"id": "actor-other", "custom_id": "someone-else"}},
					"continuation_token": "",
				},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			t.Fatal("must not bind when no matching actor was recovered")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	_, err := c.EnsureActor("ws-1", "machine-42", "Philip's Laptop")
	if err == nil {
		t.Fatal("expected the original CUSTOM_ID_CONFLICT to propagate")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Code != "CUSTOM_ID_CONFLICT" {
		t.Fatalf("code=%q, want CUSTOM_ID_CONFLICT", apiErr.Code)
	}
}

// TestEnsureActor_OtherPOSTErrorPropagatesWithoutListingActors ensures the
// CUSTOM_ID_CONFLICT recovery path is only taken for that specific error
// code — any other failure from POST /api/v3/actors must propagate
// directly, without EnsureActor ever calling GET /api/v3/actors.
func TestEnsureActor_OtherPOSTErrorPropagatesWithoutListingActors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "boom", "error_code": "INTERNAL",
			})
		default:
			t.Fatalf("unexpected request %s %s (must not list actors for a non-conflict error)", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	_, err := c.EnsureActor("ws-1", "machine-42", "Philip's Laptop")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != "INTERNAL" {
		t.Fatalf("err=%v, want *APIError{Code: INTERNAL}", err)
	}
}
