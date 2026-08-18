package e2e

import (
	"strings"
	"testing"
)

func TestBuildClaudeCmd(t *testing.T) {
	task := Task{ID: "arch-001", Prompt: "where is visimap?", MaxTurns: 30, TimeoutMin: 20}
	arm := Arm{Name: "no-memory", ConfigDir: "/tmp/arm"}
	cmd := BuildClaudeCmd(task, arm, "/workspace/phoenix", "sonnet")
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"claude", "-p", "where is visimap?", "--output-format json", "--max-turns 30", "--model sonnet", "--strict-mcp-config"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--mcp-config ") && arm.Name == "no-memory" {
		t.Error("no-memory arm must not pass --mcp-config")
	}
	if cmd.Dir != "/workspace/phoenix" {
		t.Errorf("Dir = %q", cmd.Dir)
	}
	found := false
	for _, e := range cmd.Env {
		if e == "CLAUDE_CONFIG_DIR=/tmp/arm" {
			found = true
		}
	}
	if !found {
		t.Error("CLAUDE_CONFIG_DIR not set")
	}
}

func TestParseClaudeJSON(t *testing.T) {
	out := []byte(`{"type":"result","result":"the answer","usage":{"input_tokens":1200,"output_tokens":300},"extra":1}`)
	text, in, outTok, err := ParseClaudeJSON(out)
	if err != nil || text != "the answer" || in != 1200 || outTok != 300 {
		t.Errorf("got (%q,%d,%d,%v)", text, in, outTok, err)
	}
	if _, _, _, err := ParseClaudeJSON([]byte("not json")); err == nil {
		t.Error("expected error on garbage")
	}
}
