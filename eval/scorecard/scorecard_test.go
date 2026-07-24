package scorecard

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "sc.json")
	sc := Scorecard{
		GitSHA: "abc1234", Date: "2026-07-24", Suite: "l1",
		Metrics: map[string]float64{"recall@5": 0.62, "mrr": 0.41},
		PerItem: []ItemResult{{ID: "r-001", Values: map[string]float64{"first_hit_rank": 2}}},
		Env:     map[string]string{"backend": "memorylake"},
	}
	if err := Write(p, sc); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Metrics["recall@5"] != 0.62 || got.Suite != "l1" || len(got.PerItem) != 1 {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestCompareMarkdown(t *testing.T) {
	base := Scorecard{GitSHA: "aaa", Metrics: map[string]float64{"recall@5": 0.50, "tokens": 1000}}
	cur := Scorecard{GitSHA: "bbb", Metrics: map[string]float64{"recall@5": 0.75, "mrr": 0.4}}
	md := CompareMarkdown(base, cur)
	for _, want := range []string{"recall@5", "0.50", "0.75", "+50.0%", "mrr", "tokens", "—"} {
		if !strings.Contains(md, want) {
			t.Errorf("CompareMarkdown missing %q in:\n%s", want, md)
		}
	}
}
