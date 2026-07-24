// Package e2e drives the L3 end-to-end suite: headless Claude Code runs on
// the phoenix repo across memory arms, graded by an LLM judge (spec §L3).
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type Rubric struct {
	AnswerPoints []string `json:"answer_points"`
	GotchaPoints []string `json:"gotcha_points"`
	MaxScore     int      `json:"max_score"`
}

type Task struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Prompt     string `json:"prompt"`
	Rubric     Rubric `json:"rubric"`
	MaxTurns   int    `json:"max_turns"`
	TimeoutMin int    `json:"timeout_min"`
}

func LoadTasks(dir string) ([]Task, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var tasks []Task
	seen := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var t Task
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		switch {
		case t.ID == "":
			return nil, fmt.Errorf("%s: empty id", p)
		case seen[t.ID]:
			return nil, fmt.Errorf("%s: duplicate id %q", p, t.ID)
		case t.Prompt == "":
			return nil, fmt.Errorf("%s: empty prompt", p)
		case len(t.Rubric.AnswerPoints) == 0:
			return nil, fmt.Errorf("%s: no answer points", p)
		case t.Rubric.MaxScore != 10:
			return nil, fmt.Errorf("%s: max_score must be 10", p)
		case t.MaxTurns <= 0 || t.TimeoutMin <= 0:
			return nil, fmt.Errorf("%s: max_turns and timeout_min must be > 0", p)
		}
		seen[t.ID] = true
		tasks = append(tasks, t)
	}
	return tasks, nil
}
