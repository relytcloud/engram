// Package scorecard persists versioned eval results under eval/results/
// and renders round-over-round comparisons (spec §Phase 1).
package scorecard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ItemResult struct {
	ID     string             `json:"id"`
	Values map[string]float64 `json:"values"`
	Note   string             `json:"note,omitempty"`
}

type Scorecard struct {
	GitSHA  string             `json:"git_sha"`
	Date    string             `json:"date"`
	Suite   string             `json:"suite"`
	Metrics map[string]float64 `json:"metrics"`
	PerItem []ItemResult       `json:"per_item,omitempty"`
	Env     map[string]string  `json:"env,omitempty"`
}

func Write(path string, sc Scorecard) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func Load(path string) (Scorecard, error) {
	var sc Scorecard
	b, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	err = json.Unmarshal(b, &sc)
	return sc, err
}

// CompareMarkdown renders | metric | base | current | Δ% | over the union
// of metric keys. A side missing a metric renders "—" and no delta.
func CompareMarkdown(base, cur Scorecard) string {
	keys := map[string]bool{}
	for k := range base.Metrics {
		keys[k] = true
	}
	for k := range cur.Metrics {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var b strings.Builder
	fmt.Fprintf(&b, "| metric | base (%s) | current (%s) | Δ%% |\n|---|---|---|---|\n", base.GitSHA, cur.GitSHA)
	for _, k := range sorted {
		bv, bok := base.Metrics[k]
		cv, cok := cur.Metrics[k]
		bs, cs, ds := "—", "—", "—"
		if bok {
			bs = fmt.Sprintf("%.2f", bv)
		}
		if cok {
			cs = fmt.Sprintf("%.2f", cv)
		}
		if bok && cok && bv != 0 {
			ds = fmt.Sprintf("%+.1f%%", (cv-bv)/bv*100)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", k, bs, cs, ds)
	}
	return b.String()
}
