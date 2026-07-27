package mcp

import (
	"fmt"
	"strings"
	"unicode"
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
// at searchSummaryMaxRunes on a rune boundary. Deterministic — key entities in
// the opening sentence always survive (Phase 2 spec R3 rule).
//
// A '.' only terminates when it is followed by whitespace/end-of-string and is
// not flanked by digits, so decimals survive:
// "PostgreSQL 15.3 is required. More." → "PostgreSQL 15.3 is required."
// The other terminators (!?。！？ and \n) always terminate.
//
// Known tradeoff (ACCEPTED): abbreviations whose '.' is followed by a space
// still terminate — "e.g. use flags. Done." → "e.g." — because the guard is
// deliberately context-free (digit + whitespace rules only). We do NOT carry an
// abbreviation dictionary; the summary is a scanning aid, not prose.
func firstSentence(content string) string {
	c := strings.TrimSpace(content)
	runes := []rune(c)
	for i, r := range runes {
		if r == '!' || r == '?' || r == '\n' || r == '。' || r == '！' || r == '？' {
			c = string(runes[:i+1])
			break
		}
		if r != '.' {
			continue
		}
		// Must be followed by whitespace or end-of-string. This alone keeps
		// decimals intact ("15.3" — the '3' is not whitespace).
		if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
			continue
		}
		// Must not be flanked by digits, looking past the whitespace, so a
		// spaced decimal/enumeration ("15. 3") is not a sentence end either.
		if i > 0 && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(nextNonSpace(runes[i+1:])) {
			continue
		}
		c = string(runes[:i+1])
		break
	}
	if utf8.RuneCountInString(c) > searchSummaryMaxRunes {
		runes := []rune(c)
		c = string(runes[:searchSummaryMaxRunes]) + "…"
	}
	return strings.TrimSpace(c)
}

// nextNonSpace returns the first non-whitespace rune of runes, or 0 when there
// is none (0 is not a digit, so callers treat "nothing follows" as not-a-digit).
func nextNonSpace(runes []rune) rune {
	for _, r := range runes {
		if !unicode.IsSpace(r) {
			return r
		}
	}
	return 0
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
		// Titles are user/agent-supplied and may contain newlines; flatten them
		// so the one-line-per-hit contract of the index cannot be broken.
		title := strings.ReplaceAll(r.Title, "\n", " ")
		line := fmt.Sprintf("%d. [%s] %s — %s (id: %s, ~%d tok, score %.2f)\n",
			i+1, r.Type, title, firstSentence(r.Content), r.SyncID,
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
	return renderSearchIndex(searchIndexEntries(results, budgetTokens), len(results))
}

// renderSearchIndex concatenates entry lines and appends the omission marker
// when totalResults exceeds the lines that fit.
func renderSearchIndex(lines []string, totalResults int) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
	}
	if len(lines) < totalResults {
		b.WriteString(searchOmissionLine(totalResults - len(lines)))
	}
	return b.String()
}

// searchResultEntry builds the structured envelope entry for one hit: ids,
// titles and metadata only, never the body. Shared by handleSearch and
// SearchPayloadTokens so the shipped payload and the measured payload cannot
// drift apart.
func searchResultEntry(r store.SearchResult) map[string]any {
	entry := map[string]any{
		"id":      r.ID,
		"sync_id": r.SyncID,
		"title":   r.Title,
		"type":    r.Type,
		"state":   r.State(),
		"scope":   r.Scope,
		"pinned":  r.Pinned,
	}
	if r.Project != nil {
		entry["project"] = *r.Project
	}
	if r.ReviewAfter != nil {
		entry["review_after"] = *r.ReviewAfter
	}
	return entry
}

// SearchPayloadTokens approximates the FULL agent-visible cost of a mem_search
// response at the given budget: the text index plus the structured entries of
// the hits that were actually shown. Budget-omitted hits contribute nothing —
// they ship neither text nor structured metadata — so eval (eval/runner/l1.go)
// measures exactly what handleSearch returns.
func SearchPayloadTokens(results []store.SearchResult, budgetTokens int) int {
	lines := searchIndexEntries(results, budgetTokens)
	total := approxTokens(renderSearchIndex(lines, len(results)))
	for i := range lines {
		out, err := jsonMarshal(searchResultEntry(results[i]))
		if err != nil {
			continue
		}
		total += approxTokens(string(out))
	}
	return total
}
