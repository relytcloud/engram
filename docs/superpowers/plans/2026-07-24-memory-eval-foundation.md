# Memory Eval Foundation (Phase 0 + Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the research survey (Phase 0) and the layered evaluation foundation (Phase 1: L1 retrieval quality, L2 token accounting, L3 end-to-end) defined in `docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md`, and record the baseline scorecards that Phase 2 optimization must double.

**Architecture:** A new top-level `eval/` tree inside the existing Go module (`github.com/Gentleman-Programming/engram`), so eval code imports `internal/memorylake` and `internal/store` directly. Pure logic (metrics, dataset parsing, scorecards) is unit-tested and runs under `go test ./...`; anything touching the live MemoryLake service or the `claude` CLI is env-gated (skip when credentials are absent, same pattern as `internal/paritytest.RequireMemoryLake`). One eval binary `eval/cmd/evalrun` drives all suites. The release binary is untouched (goreleaser builds only `./cmd/engram`).

**Tech Stack:** Go 1.25 (CGO_ENABLED=0), live MemoryLake V3 API via `internal/memorylake`, `claude` CLI headless (`claude -p --output-format json`) for L3.

## Global Constraints

- Every commit: `go test ./...` passes with `CGO_ENABLED=0`.
- Conventional commits; **NO `Co-Authored-By` trailers** (repo rule).
- Work on branch `feat/memory-eval-foundation` (create in Task 1, Step 1).
- Eval never ships: do not touch `.goreleaser.yaml` (`builds.main: ./cmd/engram`).
- Eval is **read-only** against the MemoryLake `phoenix` project (`proj_id=proj-52bfd150b13041389ebe2e2c66f96294`, workspace `engram`). No `mem_save`/`AddFacts` calls against it, ever.
- Live-service code paths skip (not fail) when `ENGRAM_MEMORYLAKE_API_KEY` is unset.
- Datasets are **frozen once committed** (Tasks 6, 8): never edited because of scores.
- Scorecards are committed under `eval/results/` (they are the traceable optimization log).
- Baseline anchor: engram commit `01b1e9b` semantics; scorecards record the actual `git rev-parse --short HEAD`.
- Go style: tabs, gofmt, table-driven tests, mirror existing `internal/*` idioms.

## File Structure

```
docs/research/2026-07-24-memory-sota-survey.md   # Task 1 (Phase 0)
eval/
├── metrics/metrics.go(+_test)        # recall@k, MRR, approx tokens, keyword-group hits
├── dataset/dataset.go(+_test)        # retrieval QA JSONL schema + loader/validator
├── scorecard/scorecard.go(+_test)    # scorecard JSON write/load + markdown compare
├── runner/l1.go(+_test)              # L1 runner over a SearchBackend interface
├── tokenmeter/tokenmeter.go(+_test)  # L2 static/context/composite token accounting
├── e2e/task.go(+_test)               # L3 task schema + loader
├── e2e/claude.go(+_test)             # claude -p command construction + output parsing
├── e2e/judge.go(+_test)              # judge prompt build + verdict parsing
├── e2e/arms/                         # per-arm CLAUDE_CONFIG_DIR templates
├── cmd/evalrun/main.go               # CLI: -suite l1|l2|l3|dump-facts
├── datasets/phoenix-retrieval-v1.jsonl   # Task 6 (frozen)
├── datasets/phoenix-e2e-v1/tasks/*.json  # Task 8 (frozen)
├── datasets/phoenix-e2e-v1/judge_prompt.md
└── results/                          # committed scorecards + baseline report
internal/memorylake/export_eval.go    # 1-line ListAllFacts export (Task 6)
internal/mcp/instructions_export.go   # 1-line ServerInstructions() export (Task 7)
```

---

### Task 1: Phase 0 — SOTA research survey

**Files:**
- Create: `docs/research/2026-07-24-memory-sota-survey.md`

**Interfaces:**
- Produces: the survey doc, ending with a priority matrix (candidate change × expected gain × difficulty) consumed by the Phase 2 plan. No code interfaces.

- [ ] **Step 1: Create the working branch**

```bash
cd /workspace/phoenix/engram && git checkout -b feat/memory-eval-foundation
```

- [ ] **Step 2: Dispatch 4 parallel research agents (web-enabled)**

Dispatch four research subagents concurrently, one per direction. Each agent prompt MUST end with the same output contract. Prompts (use verbatim, replacing `<DIRECTION>`):

> Research the current (2025–2026) state of the art in `<DIRECTION>`. Use web search; prefer primary sources (papers, official docs, engineering blogs). For EACH technique found, report in exactly this format:
> `### <technique name>` / `**Source**:` (URL) / `**What it does**:` (≤3 sentences) / `**Mapping to engram+MemoryLake**:` (a concrete change to the engram Go client, its memory protocol text, or a MemoryLake server-side proposal — engram is a Go memory system for coding agents: MCP `mem_save`/`mem_search`/`mem_context` tools, a ~10.3KB SessionStart protocol injection, and a MemoryLake cloud backend with semantic search over extracted facts) / `**Expected impact**:` (effect on task uplift and/or injected-token cost, with your reasoning) / `**Difficulty**: S/M/L`.
> Return raw markdown only.

The four `<DIRECTION>` values:
1. `general agent memory systems: MemGPT/Letta, Mem0, Zep/Graphiti, A-Mem, LangMem — memory organization, retrieval, forgetting/merging strategies`
2. `coding-agent-specific memory: Claude Code native memory and CLAUDE.md practice, Cursor Memories, Devin Knowledge, Windsurf memories`
3. `evaluation methodology for agent memory: LongMemEval, LoCoMo, Mem0 paper's evaluation protocol, SWE-bench-style e2e evaluation with memory`
4. `token-cost reduction for injected context: prompt-cache-friendly layout, context compression (e.g. LLMLingua), progressive/layered retrieval (index first, expand on demand), structured summaries`

- [ ] **Step 3: Synthesize into the survey doc**

Merge the four agent reports into `docs/research/2026-07-24-memory-sota-survey.md` with sections: `## 1 General agent memory`, `## 2 Coding-agent memory`, `## 3 Evaluation methodology`, `## 4 Cost-side techniques`, then `## 5 Priority matrix` — one table: `| Candidate change | Direction source | Expected gain (effect / cost) | Difficulty | Priority |`, priority = your judgment of gain÷difficulty, deduplicated across directions. Every matrix row must reference a technique section above it.

- [ ] **Step 4: Verify no placeholder rows**

Run: `grep -n "TBD\|TODO" docs/research/2026-07-24-memory-sota-survey.md`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add docs/research/2026-07-24-memory-sota-survey.md
git commit -m "docs(research): memory SOTA survey with engram-mapped priority matrix"
```

---

### Task 2: `eval/metrics` package

**Files:**
- Create: `eval/metrics/metrics.go`
- Test: `eval/metrics/metrics_test.go`

**Interfaces:**
- Produces:
  - `func RecallAtK(firstHitRanks []int, k int) float64` — ranks are 1-based; `0` means miss.
  - `func MRR(firstHitRanks []int) float64`
  - `func ApproxTokens(s string) int` — `ceil(len(bytes)/4)`, documented as approximate (accurate path is the Anthropic count-tokens API, used only in L2 when `ANTHROPIC_API_KEY` is set).
  - `func HitsKeywordGroups(text string, groups [][]string) bool` — AND across groups, OR within a group, case-insensitive substring match.

- [ ] **Step 1: Write the failing tests**

```go
package metrics

import "testing"

func TestRecallAtK(t *testing.T) {
	ranks := []int{1, 3, 0, 11} // 0 = miss
	cases := []struct {
		k    int
		want float64
	}{{1, 0.25}, {5, 0.5}, {10, 0.5}, {11, 0.75}}
	for _, c := range cases {
		if got := RecallAtK(ranks, c.k); got != c.want {
			t.Errorf("RecallAtK(k=%d) = %v, want %v", c.k, got, c.want)
		}
	}
	if got := RecallAtK(nil, 5); got != 0 {
		t.Errorf("RecallAtK(empty) = %v, want 0", got)
	}
}

func TestMRR(t *testing.T) {
	got := MRR([]int{1, 2, 0, 4}) // (1 + 0.5 + 0 + 0.25) / 4
	if want := 0.4375; got != want {
		t.Errorf("MRR = %v, want %v", got, want)
	}
}

func TestApproxTokens(t *testing.T) {
	if got := ApproxTokens("abcd"); got != 1 {
		t.Errorf("ApproxTokens(4 bytes) = %d, want 1", got)
	}
	if got := ApproxTokens("abcde"); got != 2 {
		t.Errorf("ApproxTokens(5 bytes) = %d, want 2", got)
	}
	if got := ApproxTokens(""); got != 0 {
		t.Errorf("ApproxTokens(empty) = %d, want 0", got)
	}
}

func TestHitsKeywordGroups(t *testing.T) {
	text := "ZDB stores visimap metadata in FDB via GMetaService"
	if !HitsKeywordGroups(text, [][]string{{"visimap"}, {"fdb", "foundationdb"}}) {
		t.Error("expected hit: both groups satisfied")
	}
	if HitsKeywordGroups(text, [][]string{{"visimap"}, {"parquet"}}) {
		t.Error("expected miss: second group unsatisfied")
	}
	if !HitsKeywordGroups("VISIMAP", [][]string{{"visimap"}}) {
		t.Error("expected case-insensitive hit")
	}
	if HitsKeywordGroups(text, nil) {
		t.Error("expected miss on empty groups (vacuous hit would poison recall)")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/metrics/ -v`
Expected: FAIL — `undefined: RecallAtK` (compile error).

- [ ] **Step 3: Implement**

```go
// Package metrics holds the pure scoring functions for the eval suites
// (spec: docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md).
package metrics

import "strings"

// RecallAtK: fraction of queries whose first hit rank is within k.
// Ranks are 1-based; 0 means the query never hit.
func RecallAtK(firstHitRanks []int, k int) float64 {
	if len(firstHitRanks) == 0 {
		return 0
	}
	hits := 0
	for _, r := range firstHitRanks {
		if r > 0 && r <= k {
			hits++
		}
	}
	return float64(hits) / float64(len(firstHitRanks))
}

// MRR: mean reciprocal rank; misses (rank 0) contribute 0.
func MRR(firstHitRanks []int) float64 {
	if len(firstHitRanks) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range firstHitRanks {
		if r > 0 {
			sum += 1.0 / float64(r)
		}
	}
	return sum / float64(len(firstHitRanks))
}

// ApproxTokens estimates token count as ceil(bytes/4). Fast path only —
// L2 uses the Anthropic count-tokens API when ANTHROPIC_API_KEY is set.
func ApproxTokens(s string) int {
	return (len(s) + 3) / 4
}

// HitsKeywordGroups reports whether text satisfies every group (AND),
// where a group is satisfied by any of its keywords (OR), matched as a
// case-insensitive substring. Empty groups never hit.
func HitsKeywordGroups(text string, groups [][]string) bool {
	if len(groups) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, group := range groups {
		ok := false
		for _, kw := range group {
			if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/metrics/ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add eval/metrics/
git commit -m "feat(eval): metrics package (recall@k, MRR, approx tokens, keyword-group hits)"
```

---

### Task 3: `eval/dataset` package

**Files:**
- Create: `eval/dataset/dataset.go`
- Test: `eval/dataset/dataset_test.go`

**Interfaces:**
- Produces:
  - `type RetrievalCase struct { ID, Question string; ExpectedKeywords [][]string; ExpectedFactHint, Category string }` (JSON tags: `id`, `question`, `expected_keywords`, `expected_fact_hint`, `category`)
  - `func LoadRetrieval(path string) ([]RetrievalCase, error)` — JSONL, skips blank lines, validates: unique non-empty `id`, non-empty `question`, ≥1 keyword group, no empty group.

- [ ] **Step 1: Write the failing tests**

```go
package dataset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ds.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRetrievalValid(t *testing.T) {
	p := writeTemp(t, `{"id":"r-001","question":"where is visimap metadata stored?","expected_keywords":[["visimap"],["fdb","foundationdb"]],"category":"architecture"}

{"id":"r-002","question":"what must be regenerated after rebase?","expected_keywords":[["delta_kernel_ffi.h","ffi header"]],"expected_fact_hint":"CLAUDE.md pre-build","category":"gotcha"}
`)
	cases, err := LoadRetrieval(p)
	if err != nil {
		t.Fatalf("LoadRetrieval: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("got %d cases, want 2", len(cases))
	}
	if cases[0].ID != "r-001" || len(cases[0].ExpectedKeywords) != 2 {
		t.Errorf("case 0 parsed wrong: %+v", cases[0])
	}
	if cases[1].ExpectedFactHint != "CLAUDE.md pre-build" {
		t.Errorf("case 1 hint parsed wrong: %+v", cases[1])
	}
}

func TestLoadRetrievalRejectsBad(t *testing.T) {
	bad := map[string]string{
		"dup id":       `{"id":"a","question":"q","expected_keywords":[["k"]],"category":"c"}` + "\n" + `{"id":"a","question":"q2","expected_keywords":[["k"]],"category":"c"}`,
		"empty id":     `{"id":"","question":"q","expected_keywords":[["k"]],"category":"c"}`,
		"no question":  `{"id":"a","question":"","expected_keywords":[["k"]],"category":"c"}`,
		"no keywords":  `{"id":"a","question":"q","expected_keywords":[],"category":"c"}`,
		"empty group":  `{"id":"a","question":"q","expected_keywords":[[]],"category":"c"}`,
		"invalid json": `{not json}`,
	}
	for name, content := range bad {
		if _, err := LoadRetrieval(writeTemp(t, content)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		} else if !strings.Contains(err.Error(), "line 1") && name != "dup id" {
			t.Errorf("%s: error should cite the line, got: %v", name, err)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/dataset/ -v`
Expected: FAIL — `undefined: LoadRetrieval`.

- [ ] **Step 3: Implement**

```go
// Package dataset loads the frozen eval datasets
// (eval/datasets/*, spec §Phase 1).
package dataset

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type RetrievalCase struct {
	ID               string     `json:"id"`
	Question         string     `json:"question"`
	ExpectedKeywords [][]string `json:"expected_keywords"`
	ExpectedFactHint string     `json:"expected_fact_hint,omitempty"`
	Category         string     `json:"category"`
}

// LoadRetrieval parses a JSONL retrieval dataset, skipping blank lines.
// It fails loudly on the first invalid entry so a frozen dataset can
// never silently lose cases.
func LoadRetrieval(path string) ([]RetrievalCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cases []RetrievalCase
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c RetrievalCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		if err := validate(c, seen); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		seen[c.ID] = true
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cases, nil
}

func validate(c RetrievalCase, seen map[string]bool) error {
	if c.ID == "" {
		return fmt.Errorf("empty id")
	}
	if seen[c.ID] {
		return fmt.Errorf("duplicate id %q", c.ID)
	}
	if c.Question == "" {
		return fmt.Errorf("case %s: empty question", c.ID)
	}
	if len(c.ExpectedKeywords) == 0 {
		return fmt.Errorf("case %s: no keyword groups", c.ID)
	}
	for i, g := range c.ExpectedKeywords {
		if len(g) == 0 {
			return fmt.Errorf("case %s: keyword group %d empty", c.ID, i)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/dataset/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add eval/dataset/
git commit -m "feat(eval): retrieval QA dataset schema and JSONL loader"
```

---

### Task 4: `eval/scorecard` package

**Files:**
- Create: `eval/scorecard/scorecard.go`
- Test: `eval/scorecard/scorecard_test.go`

**Interfaces:**
- Produces:
  - `type ItemResult struct { ID string; Values map[string]float64; Note string }` (tags `id`, `values`, `note`)
  - `type Scorecard struct { GitSHA, Date, Suite string; Metrics map[string]float64; PerItem []ItemResult; Env map[string]string }` (tags `git_sha`, `date`, `suite`, `metrics`, `per_item`, `env`)
  - `func Write(path string, sc Scorecard) error` — pretty JSON, creates parent dirs.
  - `func Load(path string) (Scorecard, error)`
  - `func CompareMarkdown(base, cur Scorecard) string` — table `| metric | base | current | Δ% |` over the union of metric keys, sorted; missing side rendered `—`.

- [ ] **Step 1: Write the failing tests**

```go
package scorecard

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "sc.json")
	sc := Scorecard{
		GitSHA: "abc1234", Date: "2026-07-24", Suite: "l1",
		Metrics: map[string]float64{"recall@5": 0.62, "mrr": 0.41},
		PerItem: []ItemResult{{ID: "r-001", Values: map[string]float64{"first_hit_rank": 2}}},
		Env:     map[string]string{"backend": "memorylake"},
	}
	if err := Write(p, sc); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Metrics["recall@5"] != 0.62 || got.Suite != "l1" || len(got.PerItem) != 1 {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestCompareMarkdown(t *testing.T) {
	base := Scorecard{GitSHA: "aaa", Metrics: map[string]float64{"recall@5": 0.50, "tokens": 1000}}
	cur := Scorecard{GitSHA: "bbb", Metrics: map[string]float64{"recall@5": 0.75, "mrr": 0.4}}
	md := CompareMarkdown(base, cur)
	for _, want := range []string{"recall@5", "0.50", "0.75", "+50.0%", "mrr", "tokens", "—"} {
		if !strings.Contains(md, want) {
			t.Errorf("CompareMarkdown missing %q in:\n%s", want, md)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/scorecard/ -v`
Expected: FAIL — `undefined: Scorecard`.

- [ ] **Step 3: Implement**

```go
// Package scorecard persists versioned eval results under eval/results/
// and renders round-over-round comparisons (spec §Phase 1).
package scorecard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ItemResult struct {
	ID     string             `json:"id"`
	Values map[string]float64 `json:"values"`
	Note   string             `json:"note,omitempty"`
}

type Scorecard struct {
	GitSHA  string             `json:"git_sha"`
	Date    string             `json:"date"`
	Suite   string             `json:"suite"`
	Metrics map[string]float64 `json:"metrics"`
	PerItem []ItemResult       `json:"per_item,omitempty"`
	Env     map[string]string  `json:"env,omitempty"`
}

func Write(path string, sc Scorecard) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func Load(path string) (Scorecard, error) {
	var sc Scorecard
	b, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	err = json.Unmarshal(b, &sc)
	return sc, err
}

// CompareMarkdown renders | metric | base | current | Δ% | over the union
// of metric keys. A side missing a metric renders "—" and no delta.
func CompareMarkdown(base, cur Scorecard) string {
	keys := map[string]bool{}
	for k := range base.Metrics {
		keys[k] = true
	}
	for k := range cur.Metrics {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var b strings.Builder
	fmt.Fprintf(&b, "| metric | base (%s) | current (%s) | Δ%% |\n|---|---|---|---|\n", base.GitSHA, cur.GitSHA)
	for _, k := range sorted {
		bv, bok := base.Metrics[k]
		cv, cok := cur.Metrics[k]
		bs, cs, ds := "—", "—", "—"
		if bok {
			bs = fmt.Sprintf("%.2f", bv)
		}
		if cok {
			cs = fmt.Sprintf("%.2f", cv)
		}
		if bok && cok && bv != 0 {
			ds = fmt.Sprintf("%+.1f%%", (cv-bv)/bv*100)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", k, bs, cs, ds)
	}
	return b.String()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/scorecard/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add eval/scorecard/
git commit -m "feat(eval): scorecard JSON persistence and markdown comparison"
```

---

### Task 5: L1 runner + `evalrun` CLI skeleton

**Files:**
- Create: `eval/runner/l1.go`, `eval/cmd/evalrun/main.go`
- Test: `eval/runner/l1_test.go`

**Interfaces:**
- Consumes: `metrics.*` (Task 2), `dataset.RetrievalCase` (Task 3), `scorecard.*` (Task 4), `store.SearchOptions` / `store.SearchResult` (existing), `memorylake.LoadConfig() Config` / `memorylake.LoadEnablement(memorylake.DefaultEnablementPath())` / `(*Enablement).IsEnabled(project) (ProjectEntry, bool)` / `memorylake.NewBackend(cfg Config, ws, projID string) (*MemoryLakeBackend, error)` — all existing; `(*MemoryLakeBackend).Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)` satisfies `SearchBackend`.
- Produces:
  - `type SearchBackend interface { Search(query string, opts store.SearchOptions) ([]store.SearchResult, error) }`
  - `type L1Config struct { Project string; Ks []int; Limit int }`
  - `func RunL1(b SearchBackend, cases []dataset.RetrievalCase, cfg L1Config) (scorecard.Scorecard, error)`
  - CLI: `go run ./eval/cmd/evalrun -suite l1 -dataset <path> -project phoenix -out <path>`

- [ ] **Step 1: Write the failing tests (fake backend — no network)**

```go
package runner

import (
	"testing"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/internal/store"
)

type fakeBackend struct {
	responses map[string][]store.SearchResult
}

func (f *fakeBackend) Search(q string, _ store.SearchOptions) ([]store.SearchResult, error) {
	return f.responses[q], nil
}

func obs(title, content string) store.SearchResult {
	var r store.SearchResult
	r.Title = title
	r.Content = content
	return r
}

func TestRunL1(t *testing.T) {
	cases := []dataset.RetrievalCase{
		{ID: "r-001", Question: "q1", ExpectedKeywords: [][]string{{"visimap"}}},
		{ID: "r-002", Question: "q2", ExpectedKeywords: [][]string{{"nowhere-to-be-found"}}},
	}
	b := &fakeBackend{responses: map[string][]store.SearchResult{
		"q1": {obs("noise", "unrelated"), obs("ZDB visimap", "stored in FDB")}, // hit at rank 2
		"q2": {obs("noise", "unrelated")},                                      // miss
	}}
	sc, err := RunL1(b, cases, L1Config{Project: "phoenix", Ks: []int{1, 5}, Limit: 10})
	if err != nil {
		t.Fatalf("RunL1: %v", err)
	}
	if got := sc.Metrics["recall@1"]; got != 0 {
		t.Errorf("recall@1 = %v, want 0", got)
	}
	if got := sc.Metrics["recall@5"]; got != 0.5 {
		t.Errorf("recall@5 = %v, want 0.5", got)
	}
	if got := sc.Metrics["mrr"]; got != 0.25 {
		t.Errorf("mrr = %v, want 0.25", got)
	}
	if sc.Metrics["avg_tokens_per_query"] <= 0 {
		t.Error("avg_tokens_per_query should be > 0")
	}
	if len(sc.PerItem) != 2 || sc.PerItem[0].Values["first_hit_rank"] != 2 {
		t.Errorf("per-item results wrong: %+v", sc.PerItem)
	}
	if sc.Suite != "l1" {
		t.Errorf("suite = %q, want l1", sc.Suite)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/runner/ -v`
Expected: FAIL — `undefined: RunL1`.

- [ ] **Step 3: Implement `eval/runner/l1.go`**

```go
// Package runner executes the eval suites against a memory backend.
package runner

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Gentleman-Programming/engram/eval/dataset"
	"github.com/Gentleman-Programming/engram/eval/metrics"
	"github.com/Gentleman-Programming/engram/eval/scorecard"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// SearchBackend is the slice of the memory backend L1 exercises.
// *memorylake.MemoryLakeBackend satisfies it.
type SearchBackend interface {
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
}

type L1Config struct {
	Project string
	Ks      []int
	Limit   int
}

// RunL1 runs every retrieval case, judging hits with keyword groups over
// title+content and measuring the agent-visible payload size (JSON of the
// full result list — the closest cheap proxy for the mem_search response).
func RunL1(b SearchBackend, cases []dataset.RetrievalCase, cfg L1Config) (scorecard.Scorecard, error) {
	if cfg.Limit == 0 {
		cfg.Limit = 10
	}
	ranks := make([]int, 0, len(cases))
	items := make([]scorecard.ItemResult, 0, len(cases))
	var totalTokens float64
	latencies := make([]float64, 0, len(cases))

	for _, c := range cases {
		start := time.Now()
		results, err := b.Search(c.Question, store.SearchOptions{Project: cfg.Project, Limit: cfg.Limit})
		if err != nil {
			return scorecard.Scorecard{}, fmt.Errorf("case %s: %w", c.ID, err)
		}
		latMS := float64(time.Since(start).Milliseconds())

		rank := 0
		for i, r := range results {
			if metrics.HitsKeywordGroups(r.Title+"\n"+r.Content, c.ExpectedKeywords) {
				rank = i + 1
				break
			}
		}
		payload, _ := json.Marshal(results)
		tokens := float64(metrics.ApproxTokens(string(payload)))

		ranks = append(ranks, rank)
		totalTokens += tokens
		latencies = append(latencies, latMS)
		items = append(items, scorecard.ItemResult{
			ID: c.ID,
			Values: map[string]float64{
				"first_hit_rank": float64(rank),
				"tokens":         tokens,
				"latency_ms":     latMS,
			},
		})
	}

	m := map[string]float64{
		"mrr": metrics.MRR(ranks),
	}
	for _, k := range cfg.Ks {
		m[fmt.Sprintf("recall@%d", k)] = metrics.RecallAtK(ranks, k)
	}
	if len(cases) > 0 {
		m["avg_tokens_per_query"] = totalTokens / float64(len(cases))
		m["latency_p50_ms"] = percentile(latencies, 0.50)
		m["latency_p95_ms"] = percentile(latencies, 0.95)
	}

	return scorecard.Scorecard{
		Suite:   "l1",
		Date:    time.Now().UTC().Format("2006-01-02"),
		Metrics: m,
		PerItem: items,
	}, nil
}

func percentile(sortedIn []float64, p float64) float64 {
	if len(sortedIn) == 0 {
		return 0
	}
	vals := append([]float64(nil), sortedIn...)
	for i := 1; i < len(vals); i++ { // insertion sort: n is tiny
		for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
			vals[j], vals[j-1] = vals[j-1], vals[j]
		}
	}
	idx := int(p * float64(len(vals)-1))
	return vals[idx]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/runner/ -v`
Expected: PASS.

- [ ] **Step 5: Implement the CLI skeleton `eval/cmd/evalrun/main.go`**

```go
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
```

- [ ] **Step 6: Verify the whole module still builds and tests pass**

Run: `go build ./... && go test ./...`
Expected: PASS everywhere (evalrun compiles; no live call happens in tests).

- [ ] **Step 7: Commit**

```bash
git add eval/runner/ eval/cmd/
git commit -m "feat(eval): L1 retrieval runner and evalrun CLI skeleton"
```

---

### Task 6: Build and freeze `phoenix-retrieval-v1` dataset

**Files:**
- Create: `internal/memorylake/export_eval.go` (1-line export), `eval/datasets/phoenix-retrieval-v1.jsonl`, `eval/datasets/README.md`
- Modify: `eval/cmd/evalrun/main.go` (add `dump-facts` suite)
- Test: existing `go test ./...` (export is a pass-through; no new unit test needed)

**Interfaces:**
- Consumes: unexported `(*Client).listAllFacts(ws, projID string) ([]Fact, error)` (existing, `internal/memorylake/writequeue.go`); `(*Client).ResolveWorkspaceID` (existing).
- Produces: `func (c *Client) ListAllFacts(ws, projID string) ([]Fact, error)` — read-only export for eval dataset construction; the frozen JSONL dataset (50–100 cases).

- [ ] **Step 1: Add the export**

```go
// internal/memorylake/export_eval.go
package memorylake

// ListAllFacts exposes the read-only fact listing for eval dataset
// construction (eval/cmd/evalrun -suite dump-facts). It performs no writes.
func (c *Client) ListAllFacts(ws, projID string) ([]Fact, error) {
	return c.listAllFacts(ws, projID)
}
```

- [ ] **Step 2: Add `dump-facts` to evalrun**

In `main()`'s switch add `case "dump-facts": dumpFacts(*project)`. Implementation:

```go
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
```

Add `"encoding/json"` to imports. Run: `go build ./eval/... && go test ./...` — PASS.

- [ ] **Step 3: Dump the raw material**

Run: `go run ./eval/cmd/evalrun -suite dump-facts -project phoenix > /dev/shm/mihomo-tmp-postgres/claude-1003/-workspace-phoenix-engram/dfb6d432-993d-4256-b957-7b8da4a19762/scratchpad/phoenix-facts.jsonl`
Expected: stderr reports the fact count (>0). Also gather: `git -C /workspace/phoenix log --oneline -100` and `/workspace/phoenix/CLAUDE.md`.

- [ ] **Step 4: Author 50–100 QA cases**

Construct `eval/datasets/phoenix-retrieval-v1.jsonl` from the three sources (spec §L1): reverse-construct questions from real dumped facts (~50%), phoenix git-history decisions (~25%), CLAUDE.md gotchas (~25%). Rules:
- `category` ∈ `architecture | gotcha | decision | bugfix`.
- Keyword groups must identify the *fact*, not parrot the question's wording (avoid trivially matching any result that echoes the query).
- Every case's answer must actually exist in the dumped facts — verify by grepping the dump for each case's keywords before including it.
- IDs `r-001` … zero-padded, sequential.

Write `eval/datasets/README.md` stating: construction date, sources, the freeze rule ("never edited because of scores — fix-forward via a new versioned file `phoenix-retrieval-v2.jsonl`"), and the read-only rule.

- [ ] **Step 5: Validate the dataset parses and smoke-run L1**

Run: `go test ./eval/dataset/ && go run ./eval/cmd/evalrun -suite l1 -out /dev/shm/mihomo-tmp-postgres/claude-1003/-workspace-phoenix-engram/dfb6d432-993d-4256-b957-7b8da4a19762/scratchpad/smoke-l1.json`
Expected: scorecard prints; `recall@10 > 0` (if recall@10 is 0 the dataset or search is broken — stop and investigate before freezing). Smoke output goes to scratchpad, NOT eval/results/.

- [ ] **Step 6: Commit (freeze)**

```bash
git add internal/memorylake/export_eval.go eval/cmd/evalrun/main.go eval/datasets/
git commit -m "feat(eval): freeze phoenix-retrieval-v1 dataset (dump-facts tooling + 50-100 QA cases)"
```

---

### Task 7: L2 token meter

**Files:**
- Create: `eval/tokenmeter/tokenmeter.go`, `internal/mcp/instructions_export.go`
- Modify: `eval/cmd/evalrun/main.go` (add `l2` suite)
- Test: `eval/tokenmeter/tokenmeter_test.go`

**Interfaces:**
- Consumes: `metrics.ApproxTokens` (Task 2); unexported `serverInstructions` const (existing, `internal/mcp/mcp.go:194`); `(*memorylake.MemoryLakeBackend).FormatContext(project, scope string) (string, error)` (existing).
- Produces:
  - `func ServerInstructions() string` (in `internal/mcp`) — read-only export.
  - `type ContextBackend interface { FormatContext(project, scope string) (string, error) }`
  - `func ScriptOutputTokens(scriptPath string, env []string) (int, error)` — runs `bash scriptPath`, counts stdout tokens.
  - `func ContextTokens(b ContextBackend, project string) (int, error)`
  - `func Composite(static, contextTok int, avgSearchTokens float64, searchCallsPerSession float64) float64` — `static + context + avgSearch×calls`.
- **Documented assumption:** `searchCallsPerSession` defaults to `3.0` (flag `-search-calls`); replace with replayed real-session call counts when that data exists. Record the value used in the scorecard `Env`.

- [ ] **Step 1: Write the failing tests**

```go
package tokenmeter

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeCtx struct{ out string }

func (f fakeCtx) FormatContext(project, scope string) (string, error) { return f.out, nil }

func TestScriptOutputTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.sh")
	// 8 bytes of stdout -> 2 approx tokens
	if err := os.WriteFile(p, []byte("printf 'abcdefgh'"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ScriptOutputTokens(p, os.Environ())
	if err != nil {
		t.Fatalf("ScriptOutputTokens: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d tokens, want 2", got)
	}
}

func TestContextTokens(t *testing.T) {
	got, err := ContextTokens(fakeCtx{out: "abcd"}, "phoenix")
	if err != nil || got != 1 {
		t.Errorf("ContextTokens = %d, %v; want 1, nil", got, err)
	}
}

func TestComposite(t *testing.T) {
	if got := Composite(100, 50, 200, 3); got != 750 {
		t.Errorf("Composite = %v, want 750", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/tokenmeter/ -v`
Expected: FAIL — `undefined: ScriptOutputTokens`.

- [ ] **Step 3: Implement**

`internal/mcp/instructions_export.go`:

```go
package mcp

// ServerInstructions exposes the MCP server instructions text for the L2
// token meter (eval/tokenmeter). Read-only; the MCP server behavior is
// unchanged.
func ServerInstructions() string { return serverInstructions }
```

`eval/tokenmeter/tokenmeter.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/tokenmeter/ ./internal/mcp/`
Expected: PASS.

- [ ] **Step 5: Wire `l2` into evalrun**

Add `case "l2": runL2(*project, *out, *searchCalls, *l1Card)` with flags `searchCalls := flag.Float64("search-calls", 3.0, ...)` and `l1Card := flag.String("l1-scorecard", "", "L1 scorecard to read avg_tokens_per_query from")`. Implementation:

```go
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
			"static_hook_tokens":         float64(hookTok),
			"static_skill_tokens":        float64(skillTok),
			"static_mcp_instr_tokens":    float64(instrTok),
			"context_tokens":             float64(ctxTok),
			"avg_search_tokens":          avgSearch,
			"injected_tokens_per_session": tokenmeter.Composite(static, ctxTok, avgSearch, searchCalls),
		},
		Env: map[string]string{
			"project":               project,
			"search_calls_assumed":  fmt.Sprintf("%.1f", searchCalls),
			"tokenizer":             "approx-bytes/4",
		},
	}
	writeCard(sc, out)
}
```

Add imports (`time`, `github.com/Gentleman-Programming/engram/eval/metrics`, `github.com/Gentleman-Programming/engram/eval/tokenmeter`, `github.com/Gentleman-Programming/engram/internal/mcp`).

- [ ] **Step 6: Build + full test + live smoke (if credentials present)**

Run: `go build ./... && go test ./...` — PASS.
Live smoke: `go run ./eval/cmd/evalrun -suite l2 -out /dev/shm/mihomo-tmp-postgres/claude-1003/-workspace-phoenix-engram/dfb6d432-993d-4256-b957-7b8da4a19762/scratchpad/smoke-l2.json` — prints `injected_tokens_per_session` (expect roughly 2500+ given the 10.3KB hook alone).

- [ ] **Step 7: Commit**

```bash
git add eval/tokenmeter/ internal/mcp/instructions_export.go eval/cmd/evalrun/main.go
git commit -m "feat(eval): L2 injected-token meter (static protocol + context + search composite)"
```

---

### Task 8: L3 task schema + `phoenix-e2e-v1` task set

**Files:**
- Create: `eval/e2e/task.go`, `eval/datasets/phoenix-e2e-v1/tasks/*.json` (12–16 files), `eval/datasets/phoenix-e2e-v1/judge_prompt.md`, `eval/datasets/phoenix-e2e-v1/README.md`
- Test: `eval/e2e/task_test.go`

**Interfaces:**
- Produces:
  - `type Rubric struct { AnswerPoints []string; GotchaPoints []string; MaxScore int }` (tags `answer_points`, `gotcha_points`, `max_score`)
  - `type Task struct { ID, Category, Prompt string; Rubric Rubric; MaxTurns, TimeoutMin int }` (tags `id`, `category`, `prompt`, `rubric`, `max_turns`, `timeout_min`)
  - `func LoadTasks(dir string) ([]Task, error)` — reads `dir/*.json`, sorted by filename; validates unique non-empty IDs, non-empty prompt, ≥1 answer point, `MaxScore==10`, `MaxTurns>0`, `TimeoutMin>0`.

- [ ] **Step 1: Write the failing tests**

```go
package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTask(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validTask = `{"id":"arch-001","category":"architecture-qa",
"prompt":"Where does ZDB store visimap metadata and how is it GC'd?",
"rubric":{"answer_points":["visimap keys live in FDB","GC via drop flags"],"gotcha_points":[],"max_score":10},
"max_turns":30,"timeout_min":20}`

func TestLoadTasksValid(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "arch-001.json", validTask)
	tasks, err := LoadTasks(dir)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "arch-001" || tasks[0].Rubric.MaxScore != 10 {
		t.Errorf("parsed wrong: %+v", tasks)
	}
}

func TestLoadTasksRejectsBad(t *testing.T) {
	bad := []string{
		`{"id":"","category":"c","prompt":"p","rubric":{"answer_points":["a"],"max_score":10},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"","rubric":{"answer_points":["a"],"max_score":10},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"p","rubric":{"answer_points":[],"max_score":10},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"p","rubric":{"answer_points":["a"],"max_score":5},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"p","rubric":{"answer_points":["a"],"max_score":10},"max_turns":0,"timeout_min":20}`,
	}
	for i, content := range bad {
		dir := t.TempDir()
		writeTask(t, dir, "t.json", content)
		if _, err := LoadTasks(dir); err == nil {
			t.Errorf("bad[%d]: expected error", i)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/e2e/ -v`
Expected: FAIL — `undefined: LoadTasks`.

- [ ] **Step 3: Implement `eval/e2e/task.go`**

```go
// Package e2e drives the L3 end-to-end suite: headless Claude Code runs on
// the phoenix repo across memory arms, graded by an LLM judge (spec §L3).
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type Rubric struct {
	AnswerPoints []string `json:"answer_points"`
	GotchaPoints []string `json:"gotcha_points"`
	MaxScore     int      `json:"max_score"`
}

type Task struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Prompt     string `json:"prompt"`
	Rubric     Rubric `json:"rubric"`
	MaxTurns   int    `json:"max_turns"`
	TimeoutMin int    `json:"timeout_min"`
}

func LoadTasks(dir string) ([]Task, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var tasks []Task
	seen := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var t Task
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		switch {
		case t.ID == "":
			return nil, fmt.Errorf("%s: empty id", p)
		case seen[t.ID]:
			return nil, fmt.Errorf("%s: duplicate id %q", p, t.ID)
		case t.Prompt == "":
			return nil, fmt.Errorf("%s: empty prompt", p)
		case len(t.Rubric.AnswerPoints) == 0:
			return nil, fmt.Errorf("%s: no answer points", p)
		case t.Rubric.MaxScore != 10:
			return nil, fmt.Errorf("%s: max_score must be 10", p)
		case t.MaxTurns <= 0 || t.TimeoutMin <= 0:
			return nil, fmt.Errorf("%s: max_turns and timeout_min must be > 0", p)
		}
		seen[t.ID] = true
		tasks = append(tasks, t)
	}
	return tasks, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/e2e/ -v`
Expected: PASS.

- [ ] **Step 5: Author the 12–16 tasks**

Per spec §L3: 4–5 `architecture-qa`, 4–5 `gotcha`, 4–6 `small-fix` tasks, mined from `/workspace/phoenix/CLAUDE.md`, phoenix git history, and the phoenix facts dump from Task 6. Requirements per task:
- Prompts phrased as a real user request, no answer leakage.
- `answer_points`: 3–6 objectively checkable statements (file paths, function names, required steps). `gotcha` tasks also fill `gotcha_points` (mistakes memory should prevent, e.g. "did NOT skip the delta-kernel FFI header regeneration").
- `small-fix` tasks reference bugs with a real fix commit; note the commit hash in the task README (NOT in the prompt).
- Defaults: `max_turns: 30`, `timeout_min: 20` (arch-qa can use 15/10).

Write `eval/datasets/phoenix-e2e-v1/judge_prompt.md`:

```markdown
You are grading a coding agent's answer on the phoenix codebase.

## Task
{{TASK_PROMPT}}

## Rubric ({{MAX_SCORE}} points total)
Answer points (award proportionally): {{ANSWER_POINTS}}
Gotcha points (deduct 2 each if violated): {{GOTCHA_POINTS}}

## Agent's final answer
{{AGENT_RESULT}}

Score strictly against the rubric. Do not reward verbosity or confident
wrong answers. Reply with ONLY this JSON:
{"score": <0-10 number>, "points_hit": ["..."], "points_missed": ["..."], "reasoning": "<≤3 sentences>"}
```

Write `eval/datasets/phoenix-e2e-v1/README.md` with: construction date, per-task source (commit hashes for small-fix tasks), the freeze rule, and the arm-comparison protocol.

- [ ] **Step 6: Validate and commit (freeze)**

Run: `go test ./eval/e2e/ -run TestLoadTasksValid` then add a one-off check: `go run ./eval/cmd/evalrun -suite l3 -dry-run` is not built yet, so instead validate via a tiny Go test in Step 1's file pointing at the real dir:

```go
func TestRealTaskSetParses(t *testing.T) {
	tasks, err := LoadTasks("../datasets/phoenix-e2e-v1/tasks")
	if err != nil {
		t.Fatalf("real task set invalid: %v", err)
	}
	if len(tasks) < 12 || len(tasks) > 16 {
		t.Errorf("task count %d outside 12–16", len(tasks))
	}
}
```

Run: `go test ./eval/e2e/ -v` — PASS.

```bash
git add eval/e2e/ eval/datasets/phoenix-e2e-v1/
git commit -m "feat(eval): L3 task schema and frozen phoenix-e2e-v1 task set"
```

---

### Task 9: L3 runner — claude driver, arms, judge

**Files:**
- Create: `eval/e2e/claude.go`, `eval/e2e/judge.go`, `eval/e2e/arms/no-memory/settings.json`, `eval/e2e/arms/memory/settings.json`, `eval/e2e/arms/memory/mcp.json`, `eval/e2e/arms/README.md`
- Modify: `eval/cmd/evalrun/main.go` (add `l3` suite)
- Test: `eval/e2e/claude_test.go`, `eval/e2e/judge_test.go`

**Interfaces:**
- Consumes: `Task` (Task 8), `scorecard.*` (Task 4).
- Produces:
  - `type Arm struct { Name, ConfigDir, EngramBin string }`
  - `func MaterializeArm(templateDir, workDir, engramBin string) (Arm, error)` — copies the template into `workDir`, substituting the literal `{{ENGRAM_BIN}}` in every `.json` file.
  - `func BuildClaudeCmd(task Task, arm Arm, phoenixDir, model string) *exec.Cmd` — builds `claude -p <prompt> --output-format json --max-turns <n> --model <model> --dangerously-skip-permissions`, `Dir=phoenixDir`, env = parent env + `CLAUDE_CONFIG_DIR=<arm.ConfigDir>`; memory arm additionally appends `--mcp-config <arm.ConfigDir>/mcp.json --strict-mcp-config`; no-memory arm appends `--strict-mcp-config` only.
  - `type RunResult struct { TaskID, Arm string; ResultText string; InputTokens, OutputTokens int; DurationS float64; TimedOut bool }`
  - `func ParseClaudeJSON(b []byte) (resultText string, inputTok, outputTok int, err error)` — parses `{"result": "...", "usage": {"input_tokens": N, "output_tokens": M}}`, tolerating extra fields.
  - `type JudgeVerdict struct { Score float64; PointsHit, PointsMissed []string; Reasoning string }` (tags `score`, `points_hit`, `points_missed`, `reasoning`)
  - `func BuildJudgePrompt(tpl string, task Task, agentResult string) string` — replaces `{{TASK_PROMPT}}`, `{{MAX_SCORE}}`, `{{ANSWER_POINTS}}`, `{{GOTCHA_POINTS}}`, `{{AGENT_RESULT}}`.
  - `func ParseJudgeJSON(s string) (JudgeVerdict, error)` — extracts the first `{...}` block (judges wrap JSON in prose), unmarshals, validates `0 ≤ score ≤ 10`.

**Isolation decision (deferred from spec, decided here):** each arm gets a fresh `CLAUDE_CONFIG_DIR` materialized from a committed template — the no-memory arm has no plugins/hooks/MCP and runs with `--strict-mcp-config` so nothing user-level leaks; the memory arm's template registers engram as a stdio MCP server via `mcp.json` (`{"mcpServers":{"engram":{"command":"{{ENGRAM_BIN}}","args":["mcp"]}}}`) and injects the memory protocol via a SessionStart hook in its `settings.json` that runs `plugin/claude-code/scripts/session-start.sh` with the arm's engram binary first on `PATH`. Baseline-vs-optimized arms differ only in `engramBin`. A mandatory pre-flight probe verifies isolation (Step 6).

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./eval/e2e/ -v`
Expected: FAIL — `undefined: BuildClaudeCmd` etc.

- [ ] **Step 3: Implement `eval/e2e/claude.go` and `eval/e2e/judge.go`**

`claude.go`:

```go
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
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+arm.ConfigDir)
	if arm.EngramBin != "" {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(arm.EngramBin)+":"+os.Getenv("PATH"))
	}
	return cmd
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
```

`judge.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./eval/e2e/ -v`
Expected: PASS.

- [ ] **Step 5: Create the arm templates**

`eval/e2e/arms/no-memory/settings.json`:

```json
{
  "hooks": {}
}
```

`eval/e2e/arms/memory/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash /workspace/phoenix/engram/plugin/claude-code/scripts/session-start.sh"
          }
        ]
      }
    ]
  }
}
```

`eval/e2e/arms/memory/mcp.json`:

```json
{
  "mcpServers": {
    "engram": {
      "command": "{{ENGRAM_BIN}}",
      "args": ["mcp"]
    }
  }
}
```

`eval/e2e/arms/README.md`: document that arms are templates materialized per run, the `{{ENGRAM_BIN}}` substitution, and that baseline/optimized arms are both `memory` materialized with different binaries.

- [ ] **Step 6: Wire `l3` into evalrun**

Add flags: `arms := flag.String("arms", "no-memory,memory", ...)`, `engramBin := flag.String("engram-bin", "", "engram binary for memory arms")`, `n := flag.Int("n", 1, "runs per task per arm")`, `model := flag.String("model", "sonnet", ...)`, `phoenixDir := flag.String("phoenix-dir", "/workspace/phoenix", ...)`, `probe := flag.Bool("probe-only", false, "run only the isolation probe")`. `case "l3": runL3(...)`. When `-dataset` is empty, l3 defaults to `eval/datasets/phoenix-e2e-v1`:

```go
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
		armList = append(armList, arm)
		probeArm(arm, phoenixDir, model) // fatal on isolation violation
	}
	if probeOnly {
		fmt.Println("isolation probes passed")
		return
	}

	perArm := map[string][]float64{}
	var items []scorecard.ItemResult
	for _, task := range tasks {
		for _, arm := range armList {
			for run := 0; run < n; run++ {
				res := execTask(task, arm, phoenixDir, model)
				verdict := judge(string(tpl), task, res, model)
				perArm[arm.Name] = append(perArm[arm.Name], verdict.Score)
				items = append(items, scorecard.ItemResult{
					ID: fmt.Sprintf("%s/%s/run%d", task.ID, arm.Name, run),
					Values: map[string]float64{
						"score":         verdict.Score,
						"input_tokens":  float64(res.InputTokens),
						"output_tokens": float64(res.OutputTokens),
						"duration_s":    res.DurationS,
						"timed_out":     boolToF(res.TimedOut),
					},
					Note: verdict.Reasoning,
				})
			}
		}
	}

	m := map[string]float64{}
	for name, scores := range perArm {
		m["mean_score_"+name] = mean(scores)
	}
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

func judge(tpl string, task e2e.Task, res e2e.RunResult, model string) e2e.JudgeVerdict {
	if res.TimedOut || res.ResultText == "" {
		return e2e.JudgeVerdict{Score: 0, Reasoning: "timed out or empty result"}
	}
	prompt := e2e.BuildJudgePrompt(tpl, task, res.ResultText)
	out, err := exec.Command("claude", "-p", prompt, "--output-format", "json", "--model", model, "--max-turns", "1").Output()
	if err != nil {
		fatal("judge run: %v", err)
	}
	text, _, _, err := e2e.ParseClaudeJSON(out)
	if err != nil {
		fatal("judge output: %v", err)
	}
	v, err := e2e.ParseJudgeJSON(text)
	if err != nil {
		fatal("judge verdict: %v", err)
	}
	return v
}

func probeArm(arm e2e.Arm, phoenixDir, model string) {
	probe := e2e.Task{ID: "probe", Prompt: "Reply with ONLY a comma-separated list of your available tool names.", MaxTurns: 1, TimeoutMin: 3}
	res := execTask(probe, arm, phoenixDir, model)
	hasMem := strings.Contains(res.ResultText, "mem_")
	if arm.Name == "no-memory" && hasMem {
		fatal("ISOLATION VIOLATION: no-memory arm exposes mem_* tools")
	}
	if arm.Name != "no-memory" && !hasMem {
		fatal("arm %q has no mem_* tools — MCP wiring broken", arm.Name)
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
```

Add imports: `bytes`, `path/filepath`, `strconv`, `github.com/Gentleman-Programming/engram/eval/e2e`.

- [ ] **Step 7: Add the `l1-verify` suite (spec's LLM fallback for L1 hit judgment)**

Spec §L1 requires "keyword-group matching first, LLM verification only for borderline cases". Borderline = cases whose `first_hit_rank` is 0 in a keyword-judged L1 scorecard. Add `case "l1-verify": runL1Verify(*dsPath, *project, *l1Card, *model)` to evalrun:

```go
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
```

Add imports as needed (`encoding/json`, `strings`, `github.com/Gentleman-Programming/engram/internal/store`). Keyword metrics stay primary (deterministic, free); `*_llm` metrics are the adjusted view reported alongside them.

- [ ] **Step 8: Add the `judge-calibrate` suite and run calibration**

Create 3 fixture answers for one architecture task (known good / partial / bad) as `eval/e2e/testdata/calibration/{good,partial,bad}.md`. Add `case "judge-calibrate": judgeCalibrate(*dsPath, *taskID, *model)` with flag `taskID := flag.String("task", "", "task id for judge-calibrate")`:

```go
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
```

Run: `go run ./eval/cmd/evalrun -suite judge-calibrate -task arch-001`
Acceptance: `score(good) > score(partial) > score(bad)`, with `score(good) ≥ 7` and `score(bad) ≤ 3`. Record the three scores in `eval/e2e/testdata/calibration/README.md`. If ordering fails, fix the judge prompt wording — pre-freeze prompt tuning is allowed until this step passes, then `judge_prompt.md` freezes with the dataset.

- [ ] **Step 9: Build + full tests**

Run: `go build ./... && go test ./...` — PASS.

- [ ] **Step 10: Isolation probe against the real environment**

```bash
go build -o /dev/shm/mihomo-tmp-postgres/claude-1003/-workspace-phoenix-engram/dfb6d432-993d-4256-b957-7b8da4a19762/scratchpad/engram-baseline ./cmd/engram
go run ./eval/cmd/evalrun -suite l3 -probe-only -engram-bin /dev/shm/mihomo-tmp-postgres/claude-1003/-workspace-phoenix-engram/dfb6d432-993d-4256-b957-7b8da4a19762/scratchpad/engram-baseline
```
Expected: `isolation probes passed`. This is the verification of the isolation decision — do not proceed to Task 10 until it passes.

- [ ] **Step 11: Commit**

```bash
git add eval/e2e/ eval/cmd/evalrun/main.go
git commit -m "feat(eval): L3 runner with isolated arms, claude driver, and calibrated LLM judge"
```

---

### Task 10: Baseline runs + baseline report

**Files:**
- Create: `eval/results/<sha>-<date>-l1.json`, `eval/results/<sha>-<date>-l2.json`, `eval/results/<sha>-<date>-l3.json`, `eval/results/baseline-report.md`

**Interfaces:**
- Consumes: everything above.
- Produces: the frozen baseline numbers Phase 2 must beat: `uplift_baseline` (L3) and `injected_tokens_per_session_baseline` (L2).

- [ ] **Step 1: Run L1 baseline**

```bash
go run ./eval/cmd/evalrun -suite l1
go run ./eval/cmd/evalrun -suite l1-verify -l1-scorecard eval/results/<sha>-<date>-l1.json
```
Expected: keyword scorecard plus the `-llm.json` adjusted scorecard in `eval/results/`, `recall@10 > 0`.

- [ ] **Step 2: Run L2 baseline (feeding L1's avg search tokens)**

```bash
go run ./eval/cmd/evalrun -suite l2 -l1-scorecard eval/results/<sha>-<date>-l1.json
```
Expected: scorecard with `injected_tokens_per_session`.

- [ ] **Step 3: Run L3 baseline (milestone cost — N=2, both arms)**

```bash
go run ./eval/cmd/evalrun -suite l3 -n 2 -engram-bin /dev/shm/mihomo-tmp-postgres/claude-1003/-workspace-phoenix-engram/dfb6d432-993d-4256-b957-7b8da4a19762/scratchpad/engram-baseline -dataset eval/datasets/phoenix-e2e-v1
```
Note: this is the expensive run (12–16 tasks × 2 arms × N=2 ≈ 48–64 headless sessions on phoenix). Expected: scorecard with `mean_score_memory`, `mean_score_no-memory`, `uplift`.

- [ ] **Step 4: Write `eval/results/baseline-report.md`**

Contents: the three scorecards' headline numbers; the two acceptance anchors in bold (`uplift_baseline = X` → Phase 2 target `≥ 2X`; `injected_tokens_per_session_baseline = Y` → Phase 2 target `≤ Y/2`); per-category L3 breakdown; anomalies observed (timeouts, judge disagreements); and explicitly listed assumptions (`search-calls=3.0`, tokenizer approx, N=2).

- [ ] **Step 5: Commit**

```bash
git add eval/results/
git commit -m "feat(eval): baseline scorecards and report (Phase 2 acceptance anchors)"
```

- [ ] **Step 6: Save the baseline anchors to engram memory**

Call `mem_save` (project `engram`, type `decision`, topic_key `eval/baseline-anchors`) recording: baseline commit, uplift value, injected-tokens value, and scorecard paths.

---

## Verification checklist (end of plan)

- `go test ./...` green with CGO_ENABLED=0 on the final commit.
- `eval/` imported by nothing under `cmd/engram` or `internal/` (except the two 1-line exports going the other direction): `grep -rn "engram/eval" cmd/ internal/` → only matches in `eval/` itself (i.e., no output for cmd/ and internal/).
- Datasets frozen: `eval/datasets/README.md` and `phoenix-e2e-v1/README.md` state the freeze + fix-forward rule.
- Baseline report names the two Phase 2 acceptance anchors.
