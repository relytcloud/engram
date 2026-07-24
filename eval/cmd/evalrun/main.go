// evalrun drives the eval suites (spec: 2026-07-24-memory-eval-optimization-design.md).
// It is NOT part of the release binary (goreleaser builds ./cmd/engram only).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/eval/runner"
	"github.com/Gentleman-Programming/engram/eval/scorecard"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

func main() {
	suite := flag.String("suite", "", "l1 | l2 | l3 | dump-facts")
	dsPath := flag.String("dataset", "", "dataset path (l1/l3)")
	project := flag.String("project", "phoenix", "engram project name")
	out := flag.String("out", "", "output scorecard path (default eval/results/<sha>-<date>-<suite>.json)")
	flag.Parse()

	switch *suite {
	case "l1":
		runL1(*dsPath, *project, *out)
	default:
		fmt.Fprintf(os.Stderr, "unknown or unimplemented suite %q\n", *suite)
		os.Exit(2)
	}
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
