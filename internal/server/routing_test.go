package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// stubMLBackend is a minimal in-memory mcp.MemoryBackend distinct from the
// server's local sqlite store, used to verify Server routes requests for an
// "enabled" project through the configured BackendSelector (SetBackendSelector)
// instead of always hitting sqlite. Mirrors cmd/engram/routing_test.go's
// stubBackend pattern for NewRoutingSelector itself — here the seam under
// test is Server.backendForProject/backendForSession/backendForObservation,
// not the selector's own resolution logic (already covered there).
type stubMLBackend struct {
	mcp.MemoryBackend // nil embed: only overridden methods below may be invoked
	sessions          map[string]*store.Session
	obs               map[int64]*store.Observation
	nextID            int64
}

func newStubMLBackend() *stubMLBackend {
	return &stubMLBackend{sessions: map[string]*store.Session{}, obs: map[int64]*store.Observation{}}
}

func (b *stubMLBackend) CreateSession(id, project, directory string) error {
	b.sessions[id] = &store.Session{ID: id, Project: project, Directory: directory, StartedAt: "now"}
	return nil
}

func (b *stubMLBackend) GetSession(id string) (*store.Session, error) {
	s, ok := b.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

func (b *stubMLBackend) EndSession(id string, summary string) error {
	s, ok := b.sessions[id]
	if !ok {
		return sql.ErrNoRows
	}
	s.Summary = &summary
	return nil
}

func (b *stubMLBackend) AddObservation(p store.AddObservationParams) (int64, error) {
	b.nextID++
	id := b.nextID
	proj := p.Project
	b.obs[id] = &store.Observation{ID: id, Type: p.Type, Title: p.Title, Content: p.Content, Project: &proj, Scope: p.Scope}
	return id, nil
}

func (b *stubMLBackend) GetObservation(id int64) (*store.Observation, error) {
	o, ok := b.obs[id]
	if !ok {
		return nil, fmt.Errorf("observation %d not found", id)
	}
	return o, nil
}

func (b *stubMLBackend) Search(query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	var out []store.SearchResult
	for _, o := range b.obs {
		if strings.Contains(o.Content, query) {
			out = append(out, store.SearchResult{Observation: *o})
		}
	}
	return out, nil
}

func (b *stubMLBackend) FormatContext(project, scope string) (string, error) {
	return "stub-context-for-" + project, nil
}

// selectorFor returns an mcp.BackendSelector routing exactly enabledProject to
// ml, everything else to sqlite — the shape of a real NewRoutingSelector
// without needing the memorylake package's network/config plumbing (that
// resolution logic is covered by cmd/engram/routing_test.go; this package
// only needs to verify Server calls sel(project) at all, and threads the
// result through the ID caches for by-ID endpoints).
func selectorFor(enabledProject string, ml mcp.MemoryBackend, sqlite mcp.MemoryBackend) mcp.BackendSelector {
	return func(project string) mcp.MemoryBackend {
		if project == enabledProject {
			return ml
		}
		return sqlite
	}
}

func TestServer_EnabledProject_CreateSessionAndAddObservation_RouteToMemoryLake(t *testing.T) {
	sqlite := newServerTestStore(t)
	ml := newStubMLBackend()
	srv := New(sqlite, 0)
	srv.SetBackendSelector(selectorFor("enabled-proj", ml, sqlite))
	h := srv.Handler()

	// Create a session under the enabled project.
	createReq := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"id":"sess-ml","project":"enabled-proj","directory":"/work"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d body=%s", createRec.Code, createRec.Body.String())
	}
	if _, ok := ml.sessions["sess-ml"]; !ok {
		t.Fatalf("expected session to be created on the MemoryLake stub, not sqlite")
	}

	// Add an observation under the same project/session.
	obsReq := httptest.NewRequest(http.MethodPost, "/observations", strings.NewReader(`{"session_id":"sess-ml","type":"note","title":"ml title","content":"ml content","project":"enabled-proj"}`))
	obsReq.Header.Set("Content-Type", "application/json")
	obsRec := httptest.NewRecorder()
	h.ServeHTTP(obsRec, obsReq)
	if obsRec.Code != http.StatusCreated {
		t.Fatalf("add observation: expected 201, got %d body=%s", obsRec.Code, obsRec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(obsRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	obsID := int64(created["id"].(float64))
	if _, ok := ml.obs[obsID]; !ok {
		t.Fatalf("expected observation to be created on the MemoryLake stub, not sqlite")
	}

	// GET /observations/{id}: must resolve back to the SAME backend (the
	// obsProject ID cache), not sqlite (which never saw this observation).
	getReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/observations/%d", obsID), nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get observation: expected 200 via MemoryLake routing, got %d body=%s", getRec.Code, getRec.Body.String())
	}

	// GET /sessions/{id}: must resolve back to MemoryLake (the sessionProject
	// ID cache), and EndSession similarly.
	getSessReq := httptest.NewRequest(http.MethodGet, "/sessions/sess-ml", nil)
	getSessRec := httptest.NewRecorder()
	h.ServeHTTP(getSessRec, getSessReq)
	if getSessRec.Code != http.StatusOK {
		t.Fatalf("get session: expected 200 via MemoryLake routing, got %d body=%s", getSessRec.Code, getSessRec.Body.String())
	}

	endReq := httptest.NewRequest(http.MethodPost, "/sessions/sess-ml/end", strings.NewReader(`{"summary":"done"}`))
	endReq.Header.Set("Content-Type", "application/json")
	endRec := httptest.NewRecorder()
	h.ServeHTTP(endRec, endReq)
	if endRec.Code != http.StatusOK {
		t.Fatalf("end session: expected 200, got %d body=%s", endRec.Code, endRec.Body.String())
	}
	if ml.sessions["sess-ml"].Summary == nil || *ml.sessions["sess-ml"].Summary != "done" {
		t.Fatalf("expected EndSession to land on the MemoryLake stub, got %+v", ml.sessions["sess-ml"])
	}

	// Search scoped to the enabled project must hit MemoryLake and find the
	// observation that sqlite never received.
	searchReq := httptest.NewRequest(http.MethodGet, "/search?q=ml+content&project=enabled-proj", nil)
	searchRec := httptest.NewRecorder()
	h.ServeHTTP(searchRec, searchReq)
	if searchRec.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d body=%s", searchRec.Code, searchRec.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(searchRec.Body.Bytes(), &results); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(results) != 1 || results[0]["title"] != "ml title" {
		t.Fatalf("expected search to find the MemoryLake-only observation, got %#v", results)
	}
}

// TestServer_NotEnabledProject_BehavesExactlyAsBeforeRouting is the
// non-regression guard: with a selector configured but the request's project
// NOT enabled, every call must go to the local sqlite store exactly as it did
// before SetBackendSelector existed.
func TestServer_NotEnabledProject_BehavesExactlyAsBeforeRouting(t *testing.T) {
	sqlite := newServerTestStore(t)
	ml := newStubMLBackend()
	srv := New(sqlite, 0)
	srv.SetBackendSelector(selectorFor("some-other-enabled-project", ml, sqlite))
	h := srv.Handler()

	createReq := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"id":"sess-sqlite","project":"plain-proj","directory":"/work"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d body=%s", createRec.Code, createRec.Body.String())
	}
	if len(ml.sessions) != 0 {
		t.Fatalf("expected MemoryLake stub to receive nothing for a non-enabled project, got %+v", ml.sessions)
	}

	obsReq := httptest.NewRequest(http.MethodPost, "/observations", strings.NewReader(`{"session_id":"sess-sqlite","type":"note","title":"sqlite title","content":"sqlite content","project":"plain-proj"}`))
	obsReq.Header.Set("Content-Type", "application/json")
	obsRec := httptest.NewRecorder()
	h.ServeHTTP(obsRec, obsReq)
	if obsRec.Code != http.StatusCreated {
		t.Fatalf("add observation: expected 201, got %d body=%s", obsRec.Code, obsRec.Body.String())
	}
	if len(ml.obs) != 0 {
		t.Fatalf("expected MemoryLake stub to receive no observations for a non-enabled project, got %+v", ml.obs)
	}

	searchReq := httptest.NewRequest(http.MethodGet, "/search?q=sqlite+content&project=plain-proj", nil)
	searchRec := httptest.NewRecorder()
	h.ServeHTTP(searchRec, searchReq)
	if searchRec.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d body=%s", searchRec.Code, searchRec.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(searchRec.Body.Bytes(), &results); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(results) != 1 || results[0]["title"] != "sqlite title" {
		t.Fatalf("expected sqlite-backed search to find the observation, got %#v", results)
	}
}

// TestServer_NoSelectorConfigured_UsesStoreDirectly guards the New() default:
// a Server that never had SetBackendSelector called (the vast majority of
// existing tests, and any embedder that hasn't opted in) must behave exactly
// as before this task — every call goes straight to the sqlite store.
func TestServer_NoSelectorConfigured_UsesStoreDirectly(t *testing.T) {
	sqlite := newServerTestStore(t)
	srv := New(sqlite, 0)
	h := srv.Handler()

	createReq := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"id":"sess-default","project":"any-proj","directory":"/work"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d body=%s", createRec.Code, createRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sessions/sess-default", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get session: expected 200, got %d body=%s", getRec.Code, getRec.Body.String())
	}
}
