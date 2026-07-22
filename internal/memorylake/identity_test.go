package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
