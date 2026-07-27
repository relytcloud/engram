// Package runner executes the eval suites against a memory backend.
package runner

import (
	"fmt"
	"time"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/eval/metrics"
	"github.com/Gentleman-Programming/engram/eval/scorecard"
	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// SearchBackend is the slice of the memory backend L1 exercises.
// *memorylake.MemoryLakeBackend satisfies it.
type SearchBackend interface {
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
}

type L1Config struct {
	Project string
	Ks      []int
	Limit   int
}

// RunL1 runs every retrieval case, judging hits with keyword groups over
// title+content and measuring the agent-visible payload size (the budgeted
// mem_search index text plus the structured entries the handler ships, via
// mcp.SearchPayloadTokens). NOTE: this is a floor, not an exact count —
// envelope framing (header, trailer, JSON wrapper, relation annotations) is
// unmeasured; see the SearchPayloadTokens doc comment before reading gates
// off avg_tokens_per_query.
func RunL1(b SearchBackend, cases []dataset.RetrievalCase, cfg L1Config) (scorecard.Scorecard, error) {
	if cfg.Limit == 0 {
		cfg.Limit = 10
	}
	ranks := make([]int, 0, len(cases))
	items := make([]scorecard.ItemResult, 0, len(cases))
	var totalTokens float64
	var totalDR float64
	latencies := make([]float64, 0, len(cases))

	for _, c := range cases {
		start := time.Now()
		results, err := b.Search(c.Question, store.SearchOptions{Project: cfg.Project, Limit: cfg.Limit})
		if err != nil {
			return scorecard.Scorecard{}, fmt.Errorf("case %s: %w", c.ID, err)
		}
		latMS := float64(time.Since(start).Milliseconds())

		rank := 0
		for i, r := range results {
			if metrics.HitsKeywordGroups(r.Title+"\n"+r.Content, c.ExpectedKeywords) {
				rank = i + 1
				break
			}
		}
		dr := metrics.DistractorRatio(rank, len(results))
		// Measure what the agent sees at the default budget: index text PLUS
		// the structured entries of the shown hits (mcp.SearchPayloadTokens;
		// per-hit content is single-sourced with handleSearch). Envelope
		// framing is NOT counted — treat this as a floor.
		tokens := float64(mcp.SearchPayloadTokens(results, 0))

		ranks = append(ranks, rank)
		totalTokens += tokens
		totalDR += dr
		latencies = append(latencies, latMS)
		items = append(items, scorecard.ItemResult{
			ID: c.ID,
			Values: map[string]float64{
				"first_hit_rank":   float64(rank),
				"tokens":           tokens,
				"latency_ms":       latMS,
				"distractor_ratio": dr,
			},
		})
	}

	m := map[string]float64{
		"mrr": metrics.MRR(ranks),
	}
	for _, k := range cfg.Ks {
		m[fmt.Sprintf("recall@%d", k)] = metrics.RecallAtK(ranks, k)
	}
	if len(cases) > 0 {
		m["avg_tokens_per_query"] = totalTokens / float64(len(cases))
		m["avg_distractor_ratio"] = totalDR / float64(len(cases))
		m["latency_p50_ms"] = percentile(latencies, 0.50)
		m["latency_p95_ms"] = percentile(latencies, 0.95)
	}

	return scorecard.Scorecard{
		Suite:   "l1",
		Date:    time.Now().UTC().Format("2006-01-02"),
		Metrics: m,
		PerItem: items,
	}, nil
}

func percentile(sortedIn []float64, p float64) float64 {
	if len(sortedIn) == 0 {
		return 0
	}
	vals := append([]float64(nil), sortedIn...)
	for i := 1; i < len(vals); i++ { // insertion sort: n is tiny
		for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
			vals[j], vals[j-1] = vals[j-1], vals[j]
		}
	}
	idx := int(p * float64(len(vals)-1))
	return vals[idx]
}
