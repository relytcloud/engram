package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoJSONUnwrapsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing bearer")
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "ws-1"}})
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	var out struct{ ID string `json:"id"` }
	if err := c.doJSON("GET", "/api/v3/workspaces/ws-1", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "ws-1" {
		t.Fatalf("got %q", out.ID)
	}
}

func TestDoJSONMapsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "Project not found", "error_code": "NOT_FOUND"})
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	err := c.doJSON("GET", "/api/v3/x", nil, nil)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Code != "NOT_FOUND" {
		t.Fatalf("code=%q", apiErr.Code)
	}
}
