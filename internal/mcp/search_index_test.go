package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func sr(title, content string) store.SearchResult {
	var r store.SearchResult
	r.Type = "decision"
	r.Title = title
	r.Content = content
	r.SyncID = "obs-" + title
	r.Rank = 0.9
	return r
}

func TestFormatSearchIndexShape(t *testing.T) {
	out := FormatSearchIndex([]store.SearchResult{
		sr("auth-model", "We use JWT with 15m expiry. Refresh tokens rotate."),
	}, 0)
	if !strings.Contains(out, "auth-model") || !strings.Contains(out, "We use JWT with 15m expiry.") {
		t.Fatalf("index missing title/first-sentence summary: %q", out)
	}
	if strings.Contains(out, "Refresh tokens rotate") {
		t.Fatalf("index leaked full body: %q", out)
	}
	if !strings.Contains(out, "obs-auth-model") {
		t.Fatalf("index missing id: %q", out)
	}
}

func TestFormatSearchIndexBudget(t *testing.T) {
	var many []store.SearchResult
	for i := 0; i < 50; i++ {
		many = append(many, sr(strings.Repeat("t", 20), strings.Repeat("word ", 60)))
	}
	out := FormatSearchIndex(many, 100) // ~400 bytes budget
	if len(out) > 700 {                 // budget + one entry slack + omission line
		t.Fatalf("budget not enforced: %d bytes", len(out))
	}
	if !strings.Contains(out, "more omitted") {
		t.Fatalf("missing omission marker: %q", out)
	}
}

// TestHandleSearchRendersIndexNotBodies is the wiring record for R3a: the
// mem_search tool text is the index, never the observation bodies.
func TestHandleSearchRendersIndexNotBodies(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("s-idx", "idxproj", "/tmp/idxproj"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	body := "Auth uses JWT with 15m expiry. " + strings.Repeat("LEAKMARKER filler sentence body. ", 20)
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "s-idx", Type: "decision", Title: "auth model",
		Content: body, Project: "idxproj", Scope: "project",
	}); err != nil {
		t.Fatalf("add observation: %v", err)
	}

	h := handleSearch(StaticSelector(newSQLiteBackend(s)), MCPConfig{}, NewSessionActivity(10*time.Minute))
	res, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{
		Arguments: map[string]any{"query": "auth", "project": "idxproj"},
	}})
	if err != nil {
		t.Fatalf("search handler error: %v", err)
	}
	text := callResultText(t, res)

	if !strings.Contains(text, "Found 1 memories") {
		t.Fatalf("envelope count header missing: %s", text)
	}
	if !strings.Contains(text, "1. [decision] auth model — Auth uses JWT with 15m expiry.") {
		t.Fatalf("index line missing: %s", text)
	}
	if strings.Contains(text, "LEAKMARKER") {
		t.Fatalf("search leaked full body: %s", text)
	}
	if !strings.Contains(text, "mem_get_observation") {
		t.Fatalf("missing expand-deliberately instruction: %s", text)
	}
}

// TestHandleSearchBudgetArgTrimsIndex proves the optional budget arg reaches
// the formatter and that dropped hits are announced, never silently lost.
func TestHandleSearchBudgetArgTrimsIndex(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("s-bud", "budproj", "/tmp/budproj"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.AddObservation(store.AddObservationParams{
			SessionID: "s-bud", Type: "decision",
			Title:   fmt.Sprintf("budget hit %d", i),
			Content: strings.Repeat("word ", 80),
			Project: "budproj", Scope: "project",
		}); err != nil {
			t.Fatalf("add observation %d: %v", i, err)
		}
	}

	h := handleSearch(StaticSelector(newSQLiteBackend(s)), MCPConfig{}, NewSessionActivity(10*time.Minute))
	res, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{
		Arguments: map[string]any{"query": "budget", "project": "budproj", "budget": 60.0},
	}})
	if err != nil {
		t.Fatalf("search handler error: %v", err)
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "more omitted") {
		t.Fatalf("budget arg not honored (no omission marker): %s", text)
	}
	// The structured envelope still lists every hit — only the text index is trimmed.
	if !strings.Contains(text, "\"results\":[") || strings.Count(text, "\"sync_id\"") != 5 {
		t.Fatalf("structured envelope should keep all 5 hits: %s", text)
	}
}
