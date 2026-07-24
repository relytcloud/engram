// Package tokenmeter measures the memory system's injected-token overhead
// (spec §L2): static protocol + session-start context + retrieval payloads.
package tokenmeter

import (
	"fmt"
	"os/exec"

	"github.com/Gentleman-Programming/engram/eval/metrics"
)

type ContextBackend interface {
	FormatContext(project, scope string) (string, error)
}

// ScriptOutputTokens runs `bash scriptPath` with env and counts stdout
// tokens — used on plugin/claude-code/scripts/session-start.sh, whose
// stdout is exactly what the hook injects into the session.
func ScriptOutputTokens(scriptPath string, env []string) (int, error) {
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("run %s: %w", scriptPath, err)
	}
	return metrics.ApproxTokens(string(out)), nil
}

func ContextTokens(b ContextBackend, project string) (int, error) {
	s, err := b.FormatContext(project, "")
	if err != nil {
		return 0, err
	}
	return metrics.ApproxTokens(s), nil
}

// Composite is the per-session injected-token estimate:
// static protocol + session-start context + search payload × call count.
func Composite(static, contextTok int, avgSearchTokens, searchCallsPerSession float64) float64 {
	return float64(static) + float64(contextTok) + avgSearchTokens*searchCallsPerSession
}
