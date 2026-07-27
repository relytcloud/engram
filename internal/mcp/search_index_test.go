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
	// The budget bounds the WHOLE payload: the structured envelope carries only
	// the SHOWN hits, so an omitted hit costs neither text nor metadata.
	shown := strings.Count(text, "(id: obs-") // index lines only, not the trailer
	if shown == 0 || shown >= 5 {
		t.Fatalf("expected some but not all of 5 hits shown, got %d: %s", shown, text)
	}
	if !strings.Contains(text, "\"results\":[") {
		t.Fatalf("structured envelope missing: %s", text)
	}
	if got := strings.Count(text, "\"sync_id\""); got != shown {
		t.Fatalf("envelope must carry exactly the %d shown hits, got %d: %s", shown, got, text)
	}
}

// TestSearchPayloadTokensCountsStructuredEntries pins the L1 measurement
// contract: the helper measures text + shown structured entries, and
// budget-omitted hits contribute nothing.
func TestSearchPayloadTokensCountsStructuredEntries(t *testing.T) {
	var many []store.SearchResult
	for i := 0; i < 10; i++ {
		r := sr(fmt.Sprintf("hit %d", i), strings.Repeat("word ", 40))
		many = append(many, r)
	}

	// Generous budget: every hit is shown, so the payload cost must exceed the
	// text-only cost by the structured entries.
	textOnly := approxTokens(FormatSearchIndex(many, 100000))
	full := SearchPayloadTokens(many, 100000)
	if full <= textOnly {
		t.Fatalf("payload tokens (%d) must exceed text-only tokens (%d) for shown hits", full, textOnly)
	}

	// Tight budget: only the first hit survives. Omitted hits add no structured
	// cost, so the payload must equal text + exactly one entry.
	const tight = 30
	lines := searchIndexEntries(many, tight)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line under tight budget, got %d", len(lines))
	}
	entry, err := jsonMarshal(searchResultEntry(many[0]))
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	want := approxTokens(FormatSearchIndex(many, tight)) + approxTokens(string(entry))
	if got := SearchPayloadTokens(many, tight); got != want {
		t.Fatalf("tight-budget payload tokens = %d, want %d (text + 1 entry only)", got, want)
	}
}

func TestFirstSentenceTerminators(t *testing.T) {
	long := strings.Repeat("a", 300)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"decimal not a terminator", "PostgreSQL 15.3 is required. More.", "PostgreSQL 15.3 is required."},
		// ACCEPTED tradeoff: no abbreviation dictionary, so "e.g." terminates.
		{"abbreviation terminates (accepted)", "e.g. use flags. Done.", "e.g."},
		{"plain sentence", "One. Two.", "One."},
		{"spaced decimal not a terminator", "Bump to 15. 3 was old. Next.", "Bump to 15. 3 was old."},
		{"bang terminates", "Stop! Now.", "Stop!"},
		{"newline terminates", "Header\nbody text.", "Header"},
		{"cjk terminates", "使用 JWT。其他内容。", "使用 JWT。"},
		{"no terminator caps at 160 runes", long, strings.Repeat("a", 160) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstSentence(tc.in); got != tc.want {
				t.Fatalf("firstSentence(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSearchIndexFlattensTitleNewlines guards the one-line-per-hit contract:
// a newline-bearing title must not split the index line.
func TestSearchIndexFlattensTitleNewlines(t *testing.T) {
	r := sr("auth\nmodel", "We use JWT. Rest is body.")
	r.SyncID = "obs-auth-model" // real sync ids never contain newlines
	out := FormatSearchIndex([]store.SearchResult{r}, 0)
	if strings.Count(strings.TrimSuffix(out, "\n"), "\n") != 0 {
		t.Fatalf("index must stay one line per hit: %q", out)
	}
	if !strings.Contains(out, "auth model") {
		t.Fatalf("newline in title not flattened to space: %q", out)
	}
}
