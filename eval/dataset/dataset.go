// Package dataset loads the frozen eval datasets
// (eval/datasets/*, spec §Phase 1).
package dataset

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type RetrievalCase struct {
	ID               string     `json:"id"`
	Question         string     `json:"question"`
	ExpectedKeywords [][]string `json:"expected_keywords"`
	ExpectedFactHint string     `json:"expected_fact_hint,omitempty"`
	Category         string     `json:"category"`
}

// LoadRetrieval parses a JSONL retrieval dataset, skipping blank lines.
// It fails loudly on the first invalid entry so a frozen dataset can
// never silently lose cases.
func LoadRetrieval(path string) ([]RetrievalCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cases []RetrievalCase
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c RetrievalCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		if err := validate(c, seen); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		seen[c.ID] = true
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cases, nil
}

func validate(c RetrievalCase, seen map[string]bool) error {
	if c.ID == "" {
		return fmt.Errorf("empty id")
	}
	if seen[c.ID] {
		return fmt.Errorf("duplicate id %q", c.ID)
	}
	if c.Question == "" {
		return fmt.Errorf("case %s: empty question", c.ID)
	}
	if len(c.ExpectedKeywords) == 0 {
		return fmt.Errorf("case %s: no keyword groups", c.ID)
	}
	for i, g := range c.ExpectedKeywords {
		if len(g) == 0 {
			return fmt.Errorf("case %s: keyword group %d empty", c.ID, i)
		}
	}
	return nil
}
