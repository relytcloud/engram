package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
)

type JudgeVerdict struct {
	Score        float64  `json:"score"`
	PointsHit    []string `json:"points_hit"`
	PointsMissed []string `json:"points_missed"`
	Reasoning    string   `json:"reasoning"`
}

func BuildJudgePrompt(tpl string, task Task, agentResult string) string {
	bullets := func(items []string) string {
		out := make([]string, len(items))
		for i, s := range items {
			out[i] = "- " + s
		}
		return strings.Join(out, "\n")
	}
	r := strings.NewReplacer(
		"{{TASK_PROMPT}}", task.Prompt,
		"{{MAX_SCORE}}", fmt.Sprintf("%d", task.Rubric.MaxScore),
		"{{ANSWER_POINTS}}", bullets(task.Rubric.AnswerPoints),
		"{{GOTCHA_POINTS}}", bullets(task.Rubric.GotchaPoints),
		"{{AGENT_RESULT}}", agentResult,
	)
	return r.Replace(tpl)
}

// ParseJudgeJSON pulls the first {...} block out of the judge's reply
// (models often wrap JSON in prose) and validates the score range.
func ParseJudgeJSON(s string) (JudgeVerdict, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return JudgeVerdict{}, fmt.Errorf("no JSON object in judge reply")
	}
	var v JudgeVerdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge JSON: %w", err)
	}
	if v.Score < 0 || v.Score > 10 {
		return JudgeVerdict{}, fmt.Errorf("judge score %v out of range [0,10]", v.Score)
	}
	return v, nil
}
