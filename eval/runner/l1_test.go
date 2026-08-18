package runner

import (
	"testing"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/internal/store"
)

type fakeBackend struct {
	responses map[string][]store.SearchResult
}

func (f *fakeBackend) Search(q string, _ store.SearchOptions) ([]store.SearchResult, error) {
	return f.responses[q], nil
}

func obs(title, content string) store.SearchResult {
	var r store.SearchResult
	r.Title = title
	r.Content = content
	return r
}

func TestRunL1(t *testing.T) {
	cases := []dataset.RetrievalCase{
		{ID: "r-001", Question: "q1", ExpectedKeywords: [][]string{{"visimap"}}},
		{ID: "r-002", Question: "q2", ExpectedKeywords: [][]string{{"nowhere-to-be-found"}}},
	}
	b := &fakeBackend{responses: map[string][]store.SearchResult{
		"q1": {obs("noise", "unrelated"), obs("ZDB visimap", "stored in FDB")}, // hit at rank 2
		"q2": {obs("noise", "unrelated")},                                      // miss
	}}
	sc, err := RunL1(b, cases, L1Config{Project: "phoenix", Ks: []int{1, 5}, Limit: 10})
	if err != nil {
		t.Fatalf("RunL1: %v", err)
	}
	if got := sc.Metrics["recall@1"]; got != 0 {
		t.Errorf("recall@1 = %v, want 0", got)
	}
	if got := sc.Metrics["recall@5"]; got != 0.5 {
		t.Errorf("recall@5 = %v, want 0.5", got)
	}
	if got := sc.Metrics["mrr"]; got != 0.25 {
		t.Errorf("mrr = %v, want 0.25", got)
	}
	if sc.Metrics["avg_tokens_per_query"] <= 0 {
		t.Error("avg_tokens_per_query should be > 0")
	}
	if len(sc.PerItem) != 2 || sc.PerItem[0].Values["first_hit_rank"] != 2 {
		t.Errorf("per-item results wrong: %+v", sc.PerItem)
	}
	// q1 hits at rank 2 of 2 results → (2-1)/2 = 0.5; q2 misses with 1 result → 1.
	if got := sc.PerItem[0].Values["distractor_ratio"]; got != 0.5 {
		t.Errorf("per-item distractor_ratio = %v, want 0.5", got)
	}
	if got := sc.Metrics["avg_distractor_ratio"]; got != 0.75 {
		t.Errorf("avg_distractor_ratio = %v, want 0.75", got)
	}
	if sc.Suite != "l1" {
		t.Errorf("suite = %q, want l1", sc.Suite)
	}
}
