// evalrun drives the eval suites (spec: 2026-07-24-memory-eval-optimization-design.md).
// It is NOT part of the release binary (goreleaser builds ./cmd/engram only).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/eval/metrics"
	"github.com/Gentleman-Programming/engram/eval/runner"
	"github.com/Gentleman-Programming/engram/eval/scorecard"
	"github.com/Gentleman-Programming/engram/eval/tokenmeter"
	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

func main() {
	suite := flag.String("suite", "", "l1 | l2 | l3 | dump-facts")
	dsPath := flag.String("dataset", "", "dataset path (l1/l3)")
	project := flag.String("project", "phoenix", "engram project name")
	out := flag.String("out", "", "output scorecard path (default eval/results/<sha>-<date>-<suite>.json)")
	searchCalls := flag.Float64("search-calls", 3.0, "assumed mem_search calls per session (L2 composite)")
	l1Card := flag.String("l1-scorecard", "", "L1 scorecard to read avg_tokens_per_query from")
	flag.Parse()

	switch *suite {
	case "l1":
		runL1(*dsPath, *project, *out)
	case "l2":
		runL2(*project, *out, *searchCalls, *l1Card)
	case "dump-facts":
		dumpFacts(*project)
	default:
		fmt.Fprintf(os.Stderr, "unknown or unimplemented suite %q\n", *suite)
		os.Exit(2)
	}
}

func runL2(project, out string, searchCalls float64, l1Card string) {
	// Static slices: hook stdout + memory SKILL.md + MCP server instructions.
	hookTok, err := tokenmeter.ScriptOutputTokens("plugin/claude-code/scripts/session-start.sh", os.Environ())
	if err != nil {
		fatal("hook script: %v (needs engram on PATH; run from repo root)", err)
	}
	skill, err := os.ReadFile("plugin/claude-code/skills/memory/SKILL.md")
	if err != nil {
		fatal("skill file: %v", err)
	}
	skillTok := metrics.ApproxTokens(string(skill))
	instrTok := metrics.ApproxTokens(mcp.ServerInstructions())
	static := hookTok + skillTok + instrTok

	b := mustBackend(project)
	ctxTok, err := tokenmeter.ContextTokens(b, project)
	if err != nil {
		fatal("FormatContext: %v", err)
	}

	avgSearch := 0.0
	if l1Card != "" {
		l1sc, err := scorecard.Load(l1Card)
		if err != nil {
			fatal("l1 scorecard: %v", err)
		}
		avgSearch = l1sc.Metrics["avg_tokens_per_query"]
	}

	sc := scorecard.Scorecard{
		Suite: "l2", Date: time.Now().UTC().Format("2006-01-02"), GitSHA: gitSHA(),
		Metrics: map[string]float64{
			"static_hook_tokens":          float64(hookTok),
			"static_skill_tokens":         float64(skillTok),
			"static_mcp_instr_tokens":     float64(instrTok),
			"context_tokens":              float64(ctxTok),
			"avg_search_tokens":           avgSearch,
			"injected_tokens_per_session": tokenmeter.Composite(static, ctxTok, avgSearch, searchCalls),
		},
		Env: map[string]string{
			"project":              project,
			"search_calls_assumed": fmt.Sprintf("%.1f", searchCalls),
			"tokenizer":            "approx-bytes/4",
		},
	}
	writeCard(sc, out)
}

func mustBackend(project string) *memorylake.MemoryLakeBackend {
	cfg := memorylake.LoadConfig()
	if cfg.APIKey == "" {
		fatal("ENGRAM_MEMORYLAKE_API_KEY (or saved config) required for live suites")
	}
	en, err := memorylake.LoadEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		fatal("load enablement: %v", err)
	}
	entry, ok := en.IsEnabled(project)
	if !ok {
		fatal("project %q is not MemoryLake-enabled", project)
	}
	b, err := memorylake.NewBackend(cfg, cfg.Workspace, entry.ProjID)
	if err != nil {
		fatal("NewBackend: %v", err)
	}
	return b
}

func runL1(dsPath, project, out string) {
	if dsPath == "" {
		dsPath = "eval/datasets/phoenix-retrieval-v1.jsonl"
	}
	cases, err := dataset.LoadRetrieval(dsPath)
	if err != nil {
		fatal("dataset: %v", err)
	}
	b := mustBackend(project)
	sc, err := runner.RunL1(b, cases, runner.L1Config{Project: project, Ks: []int{1, 5, 10}, Limit: 10})
	if err != nil {
		fatal("RunL1: %v", err)
	}
	sc.GitSHA = gitSHA()
	sc.Env = map[string]string{"backend": "memorylake", "project": project, "dataset": dsPath}
	writeCard(sc, out)
}

func dumpFacts(project string) {
	cfg := memorylake.LoadConfig()
	if cfg.APIKey == "" {
		fatal("ENGRAM_MEMORYLAKE_API_KEY required")
	}
	en, err := memorylake.LoadEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		fatal("load enablement: %v", err)
	}
	entry, ok := en.IsEnabled(project)
	if !ok {
		fatal("project %q is not MemoryLake-enabled", project)
	}
	client := memorylake.NewClient(cfg)
	ws, err := client.ResolveWorkspaceID(cfg.Workspace)
	if err != nil {
		fatal("workspace: %v", err)
	}
	facts, err := client.ListAllFacts(ws, entry.ProjID)
	if err != nil {
		fatal("ListAllFacts: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, f := range facts {
		if err := enc.Encode(f); err != nil {
			fatal("encode: %v", err)
		}
	}
	fmt.Fprintf(os.Stderr, "%d facts\n", len(facts))
}

func writeCard(sc scorecard.Scorecard, out string) {
	if out == "" {
		out = fmt.Sprintf("eval/results/%s-%s-%s.json", sc.GitSHA, sc.Date, sc.Suite)
	}
	if err := scorecard.Write(out, sc); err != nil {
		fatal("write scorecard: %v", err)
	}
	fmt.Printf("scorecard written: %s\n", out)
	for k, v := range sc.Metrics {
		fmt.Printf("  %-24s %.3f\n", k, v)
	}
}

func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
