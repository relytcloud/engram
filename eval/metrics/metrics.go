// Package metrics holds the pure scoring functions for the eval suites
// (spec: docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md).
package metrics

import "strings"

// RecallAtK: fraction of queries whose first hit rank is within k.
// Ranks are 1-based; 0 means the query never hit.
func RecallAtK(firstHitRanks []int, k int) float64 {
	if len(firstHitRanks) == 0 {
		return 0
	}
	hits := 0
	for _, r := range firstHitRanks {
		if r > 0 && r <= k {
			hits++
		}
	}
	return float64(hits) / float64(len(firstHitRanks))
}

// MRR: mean reciprocal rank; misses (rank 0) contribute 0.
func MRR(firstHitRanks []int) float64 {
	if len(firstHitRanks) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range firstHitRanks {
		if r > 0 {
			sum += 1.0 / float64(r)
		}
	}
	return sum / float64(len(firstHitRanks))
}

// ApproxTokens estimates token count as ceil(bytes/4). This is the only
// tokenizer implemented today; an accurate count-tokens API path is
// deferred to Phase 2 (scorecards record tokenizer=approx-bytes/4).
func ApproxTokens(s string) int {
	return (len(s) + 3) / 4
}

// HitsKeywordGroups reports whether text satisfies every group (AND),
// where a group is satisfied by any of its keywords (OR), matched as a
// case-insensitive substring. Empty groups never hit.
func HitsKeywordGroups(text string, groups [][]string) bool {
	if len(groups) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, group := range groups {
		ok := false
		for _, kw := range group {
			if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// DistractorRatio: share of returned results ranked above the first hit.
// rank is 1-based (0 = miss). A miss with results counts as all-distractor
// (1.0); no results at all counts 0 (nothing was injected).
func DistractorRatio(firstHitRank, resultCount int) float64 {
	if resultCount == 0 {
		return 0
	}
	if firstHitRank == 0 {
		return 1
	}
	return float64(firstHitRank-1) / float64(resultCount)
}
