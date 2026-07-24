package e2e

import (
	"testing"
)

func TestBuildJudgePrompt(t *testing.T) {
	tpl := "T:{{TASK_PROMPT}} M:{{MAX_SCORE}} A:{{ANSWER_POINTS}} G:{{GOTCHA_POINTS}} R:{{AGENT_RESULT}}"
	task := Task{Prompt: "p", Rubric: Rubric{AnswerPoints: []string{"a1", "a2"}, GotchaPoints: []string{"g1"}, MaxScore: 10}}
	got := BuildJudgePrompt(tpl, task, "answer")
	want := "T:p M:10 A:- a1\n- a2 G:- g1 R:answer"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseJudgeJSON(t *testing.T) {
	v, err := ParseJudgeJSON("Here is my grade:\n{\"score\": 7.5, \"points_hit\": [\"a\"], \"points_missed\": [], \"reasoning\": \"ok\"}\nDone.")
	if err != nil || v.Score != 7.5 || len(v.PointsHit) != 1 {
		t.Errorf("got %+v, %v", v, err)
	}
	if _, err := ParseJudgeJSON("{\"score\": 99}"); err == nil {
		t.Error("expected range error")
	}
	if _, err := ParseJudgeJSON("no json here"); err == nil {
		t.Error("expected error")
	}
}
