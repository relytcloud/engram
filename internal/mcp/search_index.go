package mcp

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Gentleman-Programming/engram/internal/store"
)

const defaultSearchBudgetTokens = 600

// searchSummaryMaxRunes caps a summary at a rune boundary so multi-byte
// content is never cut mid-character.
const searchSummaryMaxRunes = 160

// approxTokens mirrors eval/metrics.ApproxTokens (ceil(bytes/4)); duplicated
// because internal packages must not depend on eval/.
func approxTokens(s string) int { return (len(s) + 3) / 4 }

// firstSentence returns content up to the first sentence terminator, capped
// at 160 chars on a rune boundary. Deterministic — key entities in the
// opening sentence always survive (Phase 2 spec R3 rule).
func firstSentence(content string) string {
	c := strings.TrimSpace(content)
	for i, r := range c {
		if r == '.' || r == '!' || r == '?' || r == '\n' || r == '。' || r == '！' || r == '？' {
			c = c[:i+utf8.RuneLen(r)]
			break
		}
	}
	if utf8.RuneCountInString(c) > searchSummaryMaxRunes {
		runes := []rune(c)
		c = string(runes[:searchSummaryMaxRunes]) + "…"
	}
	return strings.TrimSpace(c)
}

// searchIndexEntries renders one index line per hit and stops at the last
// whole line that fits the token budget (always emitting at least one line so
// a single oversized hit is still visible). Returns the rendered lines; the
// caller derives the omitted count from len(results)-len(lines).
//
// Split out from FormatSearchIndex so handleSearch can interleave its
// per-result relation annotations (REQ-012) after each entry line while still
// using this single rendering/budgeting implementation.
func searchIndexEntries(results []store.SearchResult, budgetTokens int) []string {
	if budgetTokens <= 0 {
		budgetTokens = defaultSearchBudgetTokens
	}
	lines := make([]string, 0, len(results))
	used := 0
	for i, r := range results {
		line := fmt.Sprintf("%d. [%s] %s — %s (id: %s, ~%d tok, score %.2f)\n",
			i+1, r.Type, r.Title, firstSentence(r.Content), r.SyncID,
			approxTokens(r.Content), r.Rank)
		if used+approxTokens(line) > budgetTokens && len(lines) > 0 {
			break
		}
		lines = append(lines, line)
		used += approxTokens(line)
	}
	return lines
}

// searchOmissionLine is the explicit marker telling the agent that hits were
// dropped for budget reasons (never silently truncate).
func searchOmissionLine(omitted int) string {
	return fmt.Sprintf("(+%d more omitted — raise budget or refine query)\n", omitted)
}

// FormatSearchIndex renders search hits as a reference-first index: the agent
// scans titles/summaries and expands chosen ids via mem_get_observation.
// budgetTokens <= 0 uses the default; entries stop at the last whole line
// under budget, with an explicit omission marker.
func FormatSearchIndex(results []store.SearchResult, budgetTokens int) string {
	lines := searchIndexEntries(results, budgetTokens)
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
	}
	if len(lines) < len(results) {
		b.WriteString(searchOmissionLine(len(results) - len(lines)))
	}
	return b.String()
}
