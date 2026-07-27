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

// answerFirstRules are the R2 behavior rules, lowercased for matching.
var answerFirstRules = []string{
	"final reply must contain the complete answer",
	"never narrate saves",
	"batch saves at task end",
}

// saveEagerLeadIn is the banned save-eager trigger-list lead-in ("call mem_save
// IMMEDIATELY after ANY of these"), which is what made agents interrupt their
// answer to narrate a save.
const saveEagerLeadIn = "immediately after any of these"

// assertAnswerFirst checks one chunk of protocol text for all three R2 rules
// and for the absence of the save-eager lead-in.
func assertAnswerFirst(t *testing.T, label, text string) {
	t.Helper()

	s := strings.ToLower(text)
	for _, must := range answerFirstRules {
		if !strings.Contains(s, must) {
			t.Errorf("%s missing answer-first rule %q", label, must)
		}
	}
	if strings.Contains(s, saveEagerLeadIn) {
		t.Errorf("%s still contains save-eager phrasing", label)
	}
}

// TestAnswerFirstRulesPresent pins the answer-first behavior rules (R2) to
// BOTH protocol modes of BOTH hook scripts, and to both SKILL files.
//
// The hook scripts are asserted per-heredoc rather than on whole-file content:
// a whole-file assertion is satisfied by the slim heredoc alone, which would
// leave the FULL branch free to drift back to save-eager phrasing unnoticed.
// Only one branch is injected per run, so each must carry R2 on its own. That
// applies to post-compaction.sh exactly as much as to session-start.sh —
// byte-identity of the slim branches (TestSlimProtocolSurvivesCompaction) says
// nothing about the full branch, which is a separate copy of the prose.
//
// The SKILL files have no branches, so whole-file assertions are correct there.
func TestAnswerFirstRulesPresent(t *testing.T) {
	root := repoRoot(t)

	// Hook scripts: assert the slim and full heredocs separately.
	for _, plugin := range []string{"claude-code", "codex"} {
		for _, script := range []string{"session-start.sh", "post-compaction.sh"} {
			path := filepath.Join(root, "plugin", plugin, "scripts", script)
			rel, _ := filepath.Rel(root, path)

			assertAnswerFirst(t, rel+" (SLIM_PROTOCOL heredoc)",
				slimProtocolHeredoc(t, path, rel))
			assertAnswerFirst(t, rel+" (PROTOCOL heredoc)",
				fullProtocolHeredoc(t, path, rel))
		}
	}

	// SKILL files: whole-file assertions, front-matter description included.
	wholeFiles := []string{
		filepath.Join(root, "plugin", "claude-code", "skills", "memory", "SKILL.md"),
		filepath.Join(root, "plugin", "codex", "skills", "memory", "SKILL.md"),
	}
	for _, path := range wholeFiles {
		rel, _ := filepath.Rel(root, path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		assertAnswerFirst(t, rel, string(b))
	}
}

// heredocBody returns the body of the FIRST `cat <<'MARKER' ... MARKER` heredoc
// in a hook script, excluding both marker lines. It anchors on the literal
// opener `<<'MARKER'` so that a bare mention of the marker word elsewhere in
// the file (or the SLIM_ prefixed variant) cannot be mistaken for the heredoc.
//
// First-match is deliberate and load-bearing, not incidental: a marker may
// legitimately appear more than once in one file. post-compaction.sh opens two
// PROTOCOL heredocs — the protocol prose first, then the numbered
// compaction-recovery steps — and callers here always want the first one.
// TestHeredocBodyPicksFirstOccurrence pins both the occurrence count and which
// block comes back, so reordering or adding a PROTOCOL heredoc fails loudly
// instead of silently pointing the answer-first assertions at the wrong text.
func heredocBody(t *testing.T, path, rel, marker string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	s := string(b)

	opener := "<<'" + marker + "'\n"
	start := strings.Index(s, opener)
	if start < 0 {
		t.Fatalf("%s: heredoc opener %q not found", rel, strings.TrimSuffix(opener, "\n"))
	}
	body := s[start+len(opener):]

	closer := "\n" + marker + "\n"
	end := strings.Index(body, closer)
	if end < 0 {
		t.Fatalf("%s: unterminated %s heredoc", rel, marker)
	}
	return body[:end]
}

// slimProtocolHeredoc returns the body of the SLIM_PROTOCOL heredoc.
func slimProtocolHeredoc(t *testing.T, path, rel string) string {
	t.Helper()
	return heredocBody(t, path, rel, "SLIM_PROTOCOL")
}

// fullProtocolHeredoc returns the body of the full PROTOCOL heredoc — the
// non-slim branch of the same hook script.
func fullProtocolHeredoc(t *testing.T, path, rel string) string {
	t.Helper()
	return heredocBody(t, path, rel, "PROTOCOL")
}

// TestHeredocBodyPicksFirstOccurrence pins heredocBody's first-match behavior
// on the one file where the marker is genuinely ambiguous: post-compaction.sh
// opens PROTOCOL twice (protocol prose, then the numbered recovery steps).
// Without this test, swapping those two blocks would quietly redirect
// TestAnswerFirstRulesPresent at the recovery steps — which contain none of the
// R2 rules and would fail confusingly, or worse, be "fixed" by weakening the
// assertion.
func TestHeredocBodyPicksFirstOccurrence(t *testing.T) {
	root := repoRoot(t)

	for _, plugin := range []string{"claude-code", "codex"} {
		path := filepath.Join(root, "plugin", plugin, "scripts", "post-compaction.sh")
		rel, _ := filepath.Rel(root, path)

		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if got := strings.Count(string(b), "<<'PROTOCOL'\n"); got != 2 {
			t.Fatalf("%s: expected 2 PROTOCOL heredocs (protocol prose + recovery steps), found %d — "+
				"re-check that fullProtocolHeredoc's first-match still selects the protocol block", rel, got)
		}

		body := fullProtocolHeredoc(t, path, rel)
		if !strings.Contains(body, "Engram Persistent Memory — ACTIVE PROTOCOL") {
			t.Errorf("%s: fullProtocolHeredoc did not return the protocol prose block; got:\n%s", rel, body)
		}
		if strings.Contains(body, "All 4 steps are MANDATORY") {
			t.Errorf("%s: fullProtocolHeredoc returned the compaction-recovery steps instead of the protocol prose", rel)
		}
	}
}

// TestSlimProtocolSurvivesCompaction asserts that the post-compaction hooks
// carry a slim branch whose protocol text is byte-identical to the
// session-start hook's. Compaction is exactly when the protocol has to be
// re-injected, so a post-compaction hook that only knows the full protocol
// would silently undo slim mode mid-session (and blow the token budget slim
// mode exists to protect). Byte identity is the cheapest defense against the
// two copies drifting apart.
func TestSlimProtocolSurvivesCompaction(t *testing.T) {
	root := repoRoot(t)

	for _, plugin := range []string{"claude-code", "codex"} {
		sessionStart := filepath.Join(root, "plugin", plugin, "scripts", "session-start.sh")
		postCompaction := filepath.Join(root, "plugin", plugin, "scripts", "post-compaction.sh")
		relStart, _ := filepath.Rel(root, sessionStart)
		relCompact, _ := filepath.Rel(root, postCompaction)

		want := slimProtocolHeredoc(t, sessionStart, relStart)
		got := slimProtocolHeredoc(t, postCompaction, relCompact)

		if got != want {
			t.Errorf("%s slim protocol drifted from %s\n--- session-start ---\n%s\n--- post-compaction ---\n%s",
				relCompact, relStart, want, got)
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
