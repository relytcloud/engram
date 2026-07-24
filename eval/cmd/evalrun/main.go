// evalrun drives the eval suites (spec: 2026-07-24-memory-eval-optimization-design.md).
// It is NOT part of the release binary (goreleaser builds ./cmd/engram only).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/eval/e2e"
	"github.com/Gentleman-Programming/engram/eval/metrics"
	"github.com/Gentleman-Programming/engram/eval/runner"
	"github.com/Gentleman-Programming/engram/eval/scorecard"
	"github.com/Gentleman-Programming/engram/eval/tokenmeter"
	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/store"
)

func main() {
	suite := flag.String("suite", "", "l1 | l1-verify | l2 | l3 | judge-calibrate | dump-facts")
	dsPath := flag.String("dataset", "", "dataset path (l1/l3)")
	project := flag.String("project", "phoenix", "engram project name")
	out := flag.String("out", "", "output scorecard path (default eval/results/<sha>-<date>-<suite>.json)")
	searchCalls := flag.Float64("search-calls", 3.0, "assumed mem_search calls per session (L2 composite)")
	l1Card := flag.String("l1-scorecard", "", "L1 scorecard to read avg_tokens_per_query from")
	arms := flag.String("arms", "no-memory,memory", "comma-separated arm names (l3)")
	engramBin := flag.String("engram-bin", "", "engram binary for memory arms (l3)")
	n := flag.Int("n", 1, "runs per task per arm (l3)")
	model := flag.String("model", "sonnet", "claude model for agent/judge runs")
	phoenixDir := flag.String("phoenix-dir", "/workspace/phoenix", "repo dir claude runs in (l3)")
	probe := flag.Bool("probe-only", false, "run only the isolation probe (l3)")
	taskID := flag.String("task", "", "task id for judge-calibrate")
	flag.Parse()

	switch *suite {
	case "l1":
		runL1(*dsPath, *project, *out)
	case "l1-verify":
		runL1Verify(*dsPath, *project, *l1Card, *model)
	case "l2":
		runL2(*project, *out, *searchCalls, *l1Card)
	case "l3":
		dsDir := *dsPath
		if dsDir == "" {
			dsDir = "eval/datasets/phoenix-e2e-v1"
		}
		runL3(dsDir, strings.Split(*arms, ","), *engramBin, *phoenixDir, *model, *out, *n, *probe)
	case "judge-calibrate":
		judgeCalibrate(*dsPath, *taskID, *model)
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

func runL3(dsDir string, armNames []string, engramBin, phoenixDir, model, out string, n int, probeOnly bool) {
	tasks, err := e2e.LoadTasks(filepath.Join(dsDir, "tasks"))
	if err != nil {
		fatal("tasks: %v", err)
	}
	tpl, err := os.ReadFile(filepath.Join(dsDir, "judge_prompt.md"))
	if err != nil {
		fatal("judge prompt: %v", err)
	}
	work, err := os.MkdirTemp("", "engram-l3-arms-")
	if err != nil {
		fatal("workdir: %v", err)
	}

	var armList []e2e.Arm
	for _, name := range armNames {
		bin := ""
		if name != "no-memory" {
			if engramBin == "" {
				fatal("-engram-bin required for arm %q", name)
			}
			bin = engramBin
		}
		arm, err := e2e.MaterializeArm(filepath.Join("eval/e2e/arms", name), work, bin)
		if err != nil {
			fatal("arm %s: %v", name, err)
		}
		provisionArmAuth(arm, phoenixDir) // trust + OAuth credentials (see below)
		armList = append(armList, arm)
		probeArm(arm, phoenixDir, model) // fatal on isolation violation
	}
	if probeOnly {
		fmt.Println("isolation probes passed")
		return
	}

	// Resolve the scorecard path now so the incremental sidecar (<out>.runs.jsonl)
	// sits next to it and survives to enable resume on the next invocation.
	if out == "" {
		out = fmt.Sprintf("eval/results/%s-%s-%s.json", gitSHA(), time.Now().UTC().Format("2006-01-02"), "l3")
	}
	sidecar := out + ".runs.jsonl"

	// Build the full work plan in deterministic order.
	var keys []e2e.TupleKey
	taskByID := map[string]e2e.Task{}
	for _, task := range tasks {
		taskByID[task.ID] = task
		for _, arm := range armList {
			for run := 0; run < n; run++ {
				keys = append(keys, e2e.TupleKey{TaskID: task.ID, Arm: arm.Name, Run: run})
			}
		}
	}

	// Replay any existing sidecar to resume.
	cp := &e2e.Checkpoint{}
	if data, err := os.ReadFile(sidecar); err == nil {
		cp, err = e2e.ParseCheckpoint(data)
		if err != nil {
			fatal("parse checkpoint %s: %v", sidecar, err)
		}
	} else if !os.IsNotExist(err) {
		fatal("read checkpoint %s: %v", sidecar, err)
	}
	complete, reJudge, toRun := cp.Classify(keys)
	fmt.Printf("resumed: %d complete, %d re-judge, %d to run\n", len(complete), len(reJudge), len(toRun))

	f, err := os.OpenFile(sidecar, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("open checkpoint %s: %v", sidecar, err)
	}
	defer f.Close()
	cw := e2e.NewCheckpointWriter(f)

	perArm := map[string][]float64{} // successful judge scores only
	judgeFailed := map[string]int{}  // per-arm judge-failed counts
	armTotal := map[string]int{}     // per-arm total items
	var items []scorecard.ItemResult
	for _, arm := range armList {
		perArm[arm.Name] = nil // ensure a mean is emitted even if every item failed
	}

	record := func(k e2e.TupleKey, res e2e.RunResult, v e2e.JudgeVerdict) {
		armTotal[k.Arm]++
		values := map[string]float64{
			"score":         v.Score,
			"input_tokens":  float64(res.InputTokens),
			"output_tokens": float64(res.OutputTokens),
			"duration_s":    res.DurationS,
			"timed_out":     boolToF(res.TimedOut),
		}
		if v.Score < 0 {
			// Judge-failed: mark the item and EXCLUDE it from the mean (counting
			// it as 0 would bias the arm downward).
			values["judge_failed"] = 1
			judgeFailed[k.Arm]++
		} else {
			perArm[k.Arm] = append(perArm[k.Arm], v.Score)
		}
		items = append(items, scorecard.ItemResult{
			ID:     fmt.Sprintf("%s/%s/run%d", k.TaskID, k.Arm, k.Run),
			Values: values,
			Note:   v.Reasoning,
		})
	}

	for _, k := range keys {
		task := taskByID[k.TaskID]
		arm := armByName(armList, k.Arm)
		switch cp.Status(k) {
		case e2e.StatusComplete:
			res, _ := cp.Run(k)
			v, _ := cp.Verdict(k)
			record(k, res, v)
		case e2e.StatusReJudge:
			res, _ := cp.Run(k)
			v := judge(string(tpl), task, res, model)
			if err := cw.WriteVerdict(k, v); err != nil {
				fatal("checkpoint verdict %s: %v", k.TaskID, err)
			}
			record(k, res, v)
		default: // StatusToRun
			res := execTask(task, arm, phoenixDir, model)
			if err := cw.WriteRun(k, res); err != nil {
				fatal("checkpoint run %s: %v", k.TaskID, err)
			}
			v := judge(string(tpl), task, res, model)
			if err := cw.WriteVerdict(k, v); err != nil {
				fatal("checkpoint verdict %s: %v", k.TaskID, err)
			}
			record(k, res, v)
		}
	}

	m := map[string]float64{}
	totalFailed := 0
	for name, scores := range perArm {
		m["mean_score_"+name] = mean(scores)
		m["judge_failed_"+name] = float64(judgeFailed[name])
		totalFailed += judgeFailed[name]
		if t := armTotal[name]; t > 0 && float64(judgeFailed[name])/float64(t) > 0.20 {
			fmt.Fprintf(os.Stderr,
				"WARNING: arm %q had %d/%d judge-failed items (>20%%) — mean_score_%s is unreliable\n",
				name, judgeFailed[name], t, name)
		}
	}
	m["judge_failed_count"] = float64(totalFailed)
	if a, b := m["mean_score_memory"], m["mean_score_no-memory"]; len(armNames) > 1 {
		m["uplift"] = a - b
	}
	sc := scorecard.Scorecard{
		Suite: "l3", Date: time.Now().UTC().Format("2006-01-02"), GitSHA: gitSHA(),
		Metrics: m, PerItem: items,
		Env: map[string]string{"model": model, "n": strconv.Itoa(n), "arms": strings.Join(armNames, ",")},
	}
	writeCard(sc, out)
}

// armByName returns the Arm with the given name from the materialized list.
func armByName(arms []e2e.Arm, name string) e2e.Arm {
	for _, a := range arms {
		if a.Name == name {
			return a
		}
	}
	fatal("internal: unknown arm %q", name)
	return e2e.Arm{}
}

func execTask(task e2e.Task, arm e2e.Arm, phoenixDir, model string) e2e.RunResult {
	cmd := e2e.BuildClaudeCmd(task, arm, phoenixDir, model)
	start := time.Now()
	done := make(chan error, 1)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fatal("start claude: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	res := e2e.RunResult{TaskID: task.ID, Arm: arm.Name}
	select {
	case <-time.After(time.Duration(task.TimeoutMin) * time.Minute):
		_ = cmd.Process.Kill()
		<-done
		res.TimedOut = true // scores 0, spec's circuit breaker
	case err := <-done:
		if err != nil {
			fmt.Fprintf(os.Stderr, "task %s arm %s: claude exited: %v\n", task.ID, arm.Name, err)
		}
		text, in, outTok, perr := e2e.ParseClaudeJSON(outBuf.Bytes())
		if perr != nil {
			fmt.Fprintf(os.Stderr, "task %s arm %s: %v\n", task.ID, arm.Name, perr)
		}
		res.ResultText, res.InputTokens, res.OutputTokens = text, in, outTok
	}
	res.DurationS = time.Since(start).Seconds()
	return res
}

// judgeFailedScore is the sentinel verdict Score used when the judge call
// cannot be completed after all retries. Items with Score < 0 are excluded
// from arm means (they are neither a 0 nor a real grade) — see runL3.
const judgeFailedScore = -1

// judge grades one run with the LLM judge. The claude call is transient-fault
// prone (the whole grid once died on the FIRST judge call with a bare
// "exit status 1"), so it retries up to 3 attempts with 10s/30s backoff,
// captures claude's stderr on ExitError, and — instead of fataling — returns a
// sentinel verdict (Score judgeFailedScore) on final failure so the caller can
// mark the item and keep the rest of the (expensive) grid.
func judge(tpl string, task e2e.Task, res e2e.RunResult, model string) e2e.JudgeVerdict {
	if res.TimedOut || res.ResultText == "" {
		return e2e.JudgeVerdict{Score: 0, Reasoning: "timed out or empty result"}
	}
	prompt := e2e.BuildJudgePrompt(tpl, task, res.ResultText)
	backoff := []time.Duration{10 * time.Second, 30 * time.Second}
	const attempts = 3
	var lastErr string
	for attempt := 0; attempt < attempts; attempt++ {
		v, err := judgeOnce(prompt, model)
		if err == "" {
			return v
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "judge task %s (attempt %d/%d): %s\n", task.ID, attempt+1, attempts, err)
		if attempt < len(backoff) {
			time.Sleep(backoff[attempt])
		}
	}
	return e2e.JudgeVerdict{
		Score:     judgeFailedScore,
		Reasoning: fmt.Sprintf("judge failed after %d attempts: %s", attempts, lastErr),
	}
}

// judgeOnce runs a single judge claude call. On success it returns the parsed
// verdict and an empty error string; on failure it returns a descriptive error
// string (claude stderr included, truncated) and a zero verdict.
func judgeOnce(prompt, model string) (e2e.JudgeVerdict, string) {
	out, err := exec.Command("claude", "-p", prompt, "--output-format", "json", "--model", model, "--max-turns", "1").Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = fmt.Sprintf("%v: %s", err, truncate(string(ee.Stderr), 500))
		}
		return e2e.JudgeVerdict{}, "judge run: " + msg
	}
	text, _, _, perr := e2e.ParseClaudeJSON(out)
	if perr != nil {
		return e2e.JudgeVerdict{}, "judge output: " + perr.Error()
	}
	v, verr := e2e.ParseJudgeJSON(text)
	if verr != nil {
		return e2e.JudgeVerdict{}, "judge verdict: " + verr.Error()
	}
	return v, ""
}

// truncate shortens s to at most n bytes, appending an ellipsis marker when cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// probeArm verifies the isolation invariant for one arm: the memory arm must
// load the engram stdio MCP server (with tools); the no-memory arm must not.
//
// Deviation from brief: the brief's probe asks the model to list its available
// tools and greps the reply for "mem_". In this environment that signal proved
// unreliable in BOTH directions — under --max-turns 1 the model confabulates
// its tool list (it omits the real MCP tools and invents non-existent ones
// like "Workflow"/"ReportFindings"), and forcing a tool call blows the turn
// budget (error_max_turns, empty result). So instead we drive a trivial 1-turn
// run with claude's own --debug-file and inspect the MCP connection log, which
// is ground truth: at startup claude either connects the engram server with
// tools or it does not, independent of anything the model says.
func probeArm(arm e2e.Arm, phoenixDir, model string) {
	dbg := filepath.Join(arm.ConfigDir, "probe-debug.log")
	_ = os.Remove(dbg)
	task := e2e.Task{ID: "probe", Prompt: "Reply with the single word OK.", MaxTurns: 1, TimeoutMin: 3}
	cmd := e2e.BuildClaudeCmd(task, arm, phoenixDir, model)
	cmd.Args = append(cmd.Args, "--debug-file", dbg)
	var sink bytes.Buffer
	cmd.Stdout, cmd.Stderr = &sink, &sink
	if err := cmd.Start(); err != nil {
		fatal("probe %s: start claude: %v", arm.Name, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(time.Duration(task.TimeoutMin) * time.Minute):
		_ = cmd.Process.Kill()
		<-done
	case <-done:
	}
	logb, err := os.ReadFile(dbg)
	if err != nil {
		fatal("probe %s: read debug log %s: %v", arm.Name, dbg, err)
	}
	hasMem := bytes.Contains(logb, []byte(`MCP server "engram": Connection established`)) &&
		bytes.Contains(logb, []byte(`"hasTools":true`))
	if arm.Name == "no-memory" && hasMem {
		fatal("ISOLATION VIOLATION: no-memory arm connected the engram MCP server")
	}
	if arm.Name != "no-memory" && !hasMem {
		fatal("arm %q did not connect the engram MCP server (hasTools) — MCP wiring broken", arm.Name)
	}
}

// provisionArmAuth prepares a freshly materialized arm's CLAUDE_CONFIG_DIR so
// the headless `claude` invocation can actually run. This is a deviation from
// the task brief, which assumed claude would run out of the box against a
// bare config dir; in this sandbox it does not, for two reasons that the
// isolation model does not intend to break:
//
//   - Trust: claude refuses to load a workspace's project settings until the
//     trust dialog is accepted. We pre-accept phoenixDir by writing
//     .claude.json (projects[phoenixDir].hasTrustDialogAccepted = true).
//   - Auth: an isolated CLAUDE_CONFIG_DIR has no OAuth credentials, so claude
//     reports "Not logged in". We copy the caller's .credentials.json from the
//     default config dir. This is the same account — it is not plugin/hook/MCP
//     state, so it does not violate the arm isolation boundary.
func provisionArmAuth(arm e2e.Arm, phoenixDir string) {
	trust := fmt.Sprintf(`{"projects":{%q:{"hasTrustDialogAccepted":true}}}`, phoenixDir)
	if err := os.WriteFile(filepath.Join(arm.ConfigDir, ".claude.json"), []byte(trust), 0o644); err != nil {
		fatal("arm %s: write trust: %v", arm.Name, err)
	}
	srcDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if srcDir == "" {
		srcDir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	creds, err := os.ReadFile(filepath.Join(srcDir, ".credentials.json"))
	if err != nil {
		fatal("arm %s: read credentials from %s: %v", arm.Name, srcDir, err)
	}
	if err := os.WriteFile(filepath.Join(arm.ConfigDir, ".credentials.json"), creds, 0o600); err != nil {
		fatal("arm %s: write credentials: %v", arm.Name, err)
	}
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func runL1Verify(dsPath, project, cardPath, model string) {
	if dsPath == "" {
		dsPath = "eval/datasets/phoenix-retrieval-v1.jsonl"
	}
	cases, err := dataset.LoadRetrieval(dsPath)
	if err != nil {
		fatal("dataset: %v", err)
	}
	byID := map[string]dataset.RetrievalCase{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	sc, err := scorecard.Load(cardPath)
	if err != nil {
		fatal("l1 scorecard: %v", err)
	}
	b := mustBackend(project)

	ranks := make([]int, 0, len(sc.PerItem))
	verified := 0
	for i, item := range sc.PerItem {
		rank := int(item.Values["first_hit_rank"])
		if rank == 0 { // borderline: keyword miss — ask the LLM
			c := byID[item.ID]
			results, err := b.Search(c.Question, store.SearchOptions{Project: project, Limit: 10})
			if err != nil {
				fatal("case %s: %v", item.ID, err)
			}
			payload, _ := json.MarshalIndent(results, "", " ")
			prompt := fmt.Sprintf("Question: %s\n\nCandidate memory search results (JSON, in rank order):\n%s\n\nDoes any result substantively answer the question? Reply with ONLY this JSON: {\"hit_rank\": <1-based rank of the first result that answers it, or 0 if none>}", c.Question, payload)
			out, err := exec.Command("claude", "-p", prompt, "--output-format", "json", "--model", model, "--max-turns", "1").Output()
			if err != nil {
				fatal("llm verify %s: %v", item.ID, err)
			}
			text, _, _, err := e2e.ParseClaudeJSON(out)
			if err != nil {
				fatal("llm verify %s: %v", item.ID, err)
			}
			var v struct {
				HitRank int `json:"hit_rank"`
			}
			start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
			if start < 0 || end <= start || json.Unmarshal([]byte(text[start:end+1]), &v) != nil {
				fatal("llm verify %s: unparseable reply %q", item.ID, text)
			}
			if v.HitRank > 0 {
				rank = v.HitRank
				sc.PerItem[i].Values["llm_verified_rank"] = float64(v.HitRank)
				verified++
			}
		}
		ranks = append(ranks, rank)
	}

	for _, k := range []int{1, 5, 10} {
		sc.Metrics[fmt.Sprintf("recall@%d_llm", k)] = metrics.RecallAtK(ranks, k)
	}
	sc.Metrics["mrr_llm"] = metrics.MRR(ranks)
	sc.Metrics["llm_verified_hits"] = float64(verified)
	sc.Suite = "l1v"
	writeCard(sc, strings.TrimSuffix(cardPath, ".json")+"-llm.json")
}

func judgeCalibrate(dsDir, taskID, model string) {
	if dsDir == "" {
		dsDir = "eval/datasets/phoenix-e2e-v1"
	}
	tasks, err := e2e.LoadTasks(filepath.Join(dsDir, "tasks"))
	if err != nil {
		fatal("tasks: %v", err)
	}
	var task e2e.Task
	for _, t := range tasks {
		if t.ID == taskID {
			task = t
		}
	}
	if task.ID == "" {
		fatal("task %q not found", taskID)
	}
	tpl, err := os.ReadFile(filepath.Join(dsDir, "judge_prompt.md"))
	if err != nil {
		fatal("judge prompt: %v", err)
	}
	for _, name := range []string{"good", "partial", "bad"} {
		answer, err := os.ReadFile(filepath.Join("eval/e2e/testdata/calibration", name+".md"))
		if err != nil {
			fatal("fixture %s: %v", name, err)
		}
		v := judge(string(tpl), task, e2e.RunResult{TaskID: task.ID, ResultText: string(answer)}, model)
		fmt.Printf("%-8s score=%.1f  %s\n", name, v.Score, v.Reasoning)
	}
}
