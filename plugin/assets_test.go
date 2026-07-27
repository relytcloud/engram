package plugin_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot returns the absolute path to the repository root by walking up from
// this test file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// plugin/assets_test.go -> up one directory
	return filepath.Dir(filepath.Dir(file))
}

// TestPluginAssetsDoNotLeakSpanishTriggers walks the injected assets of all
// three plugins (claude-code, opencode, pi) and asserts that none of them
// contain Spanish trigger tokens. Those tokens act as register cues in the
// model's context and cause English sessions to drift into Spanish even when
// language-lock rules are in place elsewhere.
func TestPluginAssetsDoNotLeakSpanishTriggers(t *testing.T) {
	root := repoRoot(t)

	bannedTokens := []string{
		`"dale"`,
		`"listo"`,
		`"acordate"`,
		`"qué hicimos"`,
		`"sí, esa"`,
		`"siempre hacé`,
		`"recordar"`,
		`"vamos con eso"`,
		`"me gusta más así"`,
		`"descartemos eso"`,
		`"quiero algo diferente"`,
	}

	targets := []struct {
		pattern string
	}{
		// claude-code: shell scripts and skill markdown files
		{filepath.Join(root, "plugin", "claude-code", "scripts", "*.sh")},
		{filepath.Join(root, "plugin", "claude-code", "skills", "*", "SKILL.md")},
		// opencode: TypeScript plugin adapter
		{filepath.Join(root, "plugin", "opencode", "*.ts")},
		// pi: TypeScript plugin adapter
		{filepath.Join(root, "plugin", "pi", "*.ts")},
	}

	for _, target := range targets {
		matches, err := filepath.Glob(target.pattern)
		if err != nil {
			t.Fatalf("glob %q: %v", target.pattern, err)
		}
		if len(matches) == 0 {
			t.Fatalf("glob %q matched no files — check the path", target.pattern)
		}
		for _, path := range matches {
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			rel, _ := filepath.Rel(root, path)
			text := string(content)
			for _, token := range bannedTokens {
				if strings.Contains(text, token) {
					t.Errorf("%s contains banned Spanish trigger token %s", rel, token)
				}
			}
		}
	}
}

// TestSlimProtocolBudgetAndCoreRules pins the compact (slim) protocol text
// emitted by the session-start hooks to a token budget and asserts the rules
// that must survive compaction. Slim mode exists to cut per-session prompt
// cost, so an unbounded slim heredoc defeats the whole point; the required
// tokens are the load-bearing parts of the protocol that cannot be deferred to
// the on-demand `memory` SKILL.
func TestSlimProtocolBudgetAndCoreRules(t *testing.T) {
	root := repoRoot(t)

	// Budget in bytes for the slim heredoc (marker line to closing marker).
	// ~3400 bytes ≈ ~850 tokens.
	const slimBudgetBytes = 3400

	scripts := []string{
		filepath.Join(root, "plugin", "claude-code", "scripts", "session-start.sh"),
		filepath.Join(root, "plugin", "codex", "scripts", "session-start.sh"),
	}

	for _, path := range scripts {
		rel, _ := filepath.Rel(root, path)

		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		s := string(b)

		start := strings.Index(s, "SLIM_PROTOCOL")
		if start < 0 {
			t.Fatalf("%s: slim branch heredoc marker SLIM_PROTOCOL not found", rel)
		}
		end := strings.Index(s[start+1:], "SLIM_PROTOCOL")
		if end < 0 {
			t.Fatalf("%s: unterminated SLIM_PROTOCOL heredoc", rel)
		}
		slim := s[start : start+1+end]

		if len(slim) > slimBudgetBytes {
			t.Fatalf("%s: slim protocol is %d bytes, budget %d", rel, len(slim), slimBudgetBytes)
		}

		for _, must := range []string{
			"mem_save", "mem_search", "mem_context", "mem_session_summary",
			"topic_key", "memorylake", "SKILL", // pointer to the on-demand skill
		} {
			if !strings.Contains(strings.ToLower(slim), strings.ToLower(must)) {
				t.Fatalf("%s: slim protocol missing required token %q", rel, must)
			}
		}
	}
}

// TestMemorySkillsCarryConflictLoop pins the conflict-resolution walkthrough to
// the on-demand `memory` SKILL files. This detail used to live in the MCP
// server instructions, which every client injects into every session; it was
// moved here because an agent needs it only when mem_save actually returns
// judgment_required, and SKILL.md is loaded on demand (its size is not in the
// per-session budget — see TestServerInstructionsBudget in internal/mcp).
//
// If these phrases disappear, the trimmed server instructions point at a SKILL
// that no longer documents the loop, and the guidance is lost entirely.
func TestMemorySkillsCarryConflictLoop(t *testing.T) {
	root := repoRoot(t)

	skills := []string{
		filepath.Join(root, "plugin", "claude-code", "skills", "memory", "SKILL.md"),
		filepath.Join(root, "plugin", "codex", "skills", "memory", "SKILL.md"),
	}

	required := []string{
		// Section header — agents must be able to grep for it
		"## Conflict loop (SQLite projects)",

		// Core trigger condition
		"judgment_required",

		// The action: iterate candidates and call mem_judge
		"candidates[]",
		"mem_judge",

		// The per-candidate judgment_id rule (not the top-level one)
		"Do NOT use the top-level judgment_id",

		// Heuristic: low confidence threshold
		"0.7",

		// Heuristic: ask for high-stakes relation+type combos
		"supersedes",
		"conflicts_with",
		"architecture",

		// Conversational (not blocking) resolution pattern
		"conversationally",

		// Post-resolution: persist via mem_judge with evidence
		"evidence",

		// The deferred tool list also moved out of the server instructions
		"mem_merge_projects",
	}

	for _, path := range skills {
		rel, _ := filepath.Rel(root, path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(b)
		for _, phrase := range required {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s is missing required phrase %q from the conflict loop section", rel, phrase)
			}
		}
	}
}

// marketplaceJSON is the minimal structure of .claude-plugin/marketplace.json
// needed to extract the version declared for the engram plugin entry.
type marketplaceJSON struct {
	Plugins []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"plugins"`
}

// pluginJSON is the structure of plugin/claude-code/.claude-plugin/plugin.json.
type pluginJSON struct {
	Version string `json:"version"`
}

// TestPluginVersionsMatch asserts that the version declared in
// .claude-plugin/marketplace.json matches the version in
// plugin/claude-code/.claude-plugin/plugin.json.
//
// A mismatch between these two files causes Claude Code to silently skip
// installation or re-download the plugin on every run because it sees the
// cached version as stale.
func TestPluginVersionsMatch(t *testing.T) {
	root := repoRoot(t)

	// Read marketplace.json
	marketplacePath := filepath.Join(root, ".claude-plugin", "marketplace.json")
	marketplaceData, err := os.ReadFile(marketplacePath)
	if err != nil {
		t.Fatalf("cannot read marketplace.json: %v", err)
	}
	var marketplace marketplaceJSON
	if err := json.Unmarshal(marketplaceData, &marketplace); err != nil {
		t.Fatalf("cannot parse marketplace.json: %v", err)
	}

	// Read plugin.json
	pluginPath := filepath.Join(root, "plugin", "claude-code", ".claude-plugin", "plugin.json")
	pluginData, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("cannot read plugin.json: %v", err)
	}
	var plugin pluginJSON
	if err := json.Unmarshal(pluginData, &plugin); err != nil {
		t.Fatalf("cannot parse plugin.json: %v", err)
	}

	// Find the engram plugin entry in marketplace.json
	var marketplaceVersion string
	for _, p := range marketplace.Plugins {
		if p.Name == "engram" {
			marketplaceVersion = p.Version
			break
		}
	}
	if marketplaceVersion == "" {
		t.Fatal("marketplace.json contains no plugin entry named 'engram'")
	}

	if marketplaceVersion != plugin.Version {
		t.Errorf(
			"plugin version mismatch: marketplace.json declares %q but plugin/claude-code/.claude-plugin/plugin.json declares %q — keep them in sync",
			marketplaceVersion,
			plugin.Version,
		)
	}
}
