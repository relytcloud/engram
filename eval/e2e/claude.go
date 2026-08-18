package e2e

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type Arm struct {
	Name      string
	ConfigDir string
	EngramBin string
}

type RunResult struct {
	TaskID       string  `json:"task_id"`
	Arm          string  `json:"arm"`
	ResultText   string  `json:"result_text"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	DurationS    float64 `json:"duration_s"`
	TimedOut     bool    `json:"timed_out"`
}

// MaterializeArm copies templateDir into workDir, substituting the literal
// {{ENGRAM_BIN}} inside every .json file, and returns the ready Arm.
func MaterializeArm(templateDir, workDir, engramBin string) (Arm, error) {
	name := filepath.Base(templateDir)
	dst := filepath.Join(workDir, name)
	err := filepath.WalkDir(templateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(templateDir, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".json") {
			b = []byte(strings.ReplaceAll(string(b), "{{ENGRAM_BIN}}", engramBin))
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		return Arm{}, err
	}
	return Arm{Name: name, ConfigDir: dst, EngramBin: engramBin}, nil
}

// BuildClaudeCmd constructs the headless Claude Code invocation for one
// task under one arm. The caller applies the timeout (exec.CommandContext
// wrapper in the runner) and captures stdout.
func BuildClaudeCmd(task Task, arm Arm, phoenixDir, model string) *exec.Cmd {
	args := []string{
		"-p", task.Prompt,
		"--output-format", "json",
		"--max-turns", strconv.Itoa(task.MaxTurns),
		"--model", model,
		"--dangerously-skip-permissions",
		"--strict-mcp-config",
	}
	if mcpCfg := filepath.Join(arm.ConfigDir, "mcp.json"); fileExists(mcpCfg) {
		args = append(args, "--mcp-config", mcpCfg)
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = phoenixDir
	// Deviation from brief: the arm's engram MCP subprocess must be free to
	// route the phoenix project to MemoryLake. The parent process runs the
	// eval suites with ENGRAM_BACKEND=sqlite (goenv.sh), which would force the
	// stdio MCP server to sqlite and break memory-arm materialization. Strip
	// ENGRAM_BACKEND from the inherited env so per-project routing decides.
	cmd.Env = stripEnv(os.Environ(), "ENGRAM_BACKEND")
	cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+arm.ConfigDir)
	if arm.EngramBin != "" {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(arm.EngramBin)+":"+os.Getenv("PATH"))
	}
	return cmd
}

// stripEnv returns env with any KEY=... entry for the given key removed.
func stripEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ParseClaudeJSON extracts the final answer and token usage from
// `claude -p --output-format json` stdout.
func ParseClaudeJSON(b []byte) (string, int, int, error) {
	var v struct {
		Result string `json:"result"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", 0, 0, fmt.Errorf("parse claude output: %w", err)
	}
	return v.Result, v.Usage.InputTokens, v.Usage.OutputTokens, nil
}
