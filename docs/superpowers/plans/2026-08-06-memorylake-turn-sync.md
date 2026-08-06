# MemoryLake 逐轮对话同步 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Claude Code 每完成一轮问答，就把这轮对话写进该项目的 MemoryLake conversation，由 MemoryLake 后台自动抽取成记忆——不依赖模型主动调 `mem_save`。

**Architecture:** Claude Code 的 `Stop` hook（每轮触发、`async: true`）调用新的 `engram turn` CLI 子命令。该命令解析 transcript JSONL 取出最后一轮，渲染成一条合并消息，走已有的 `client.AppendObservation`（`ensureConversation(custom_id = session id)` + `POST /conversations/{id}/messages`）写入。开关是 `~/.engram/memorylake.json` 里 `ProjectEntry` 上的一个 bool，默认关闭，未开启的项目在读完那个 JSON 后立即返回。

**Tech Stack:** Go（`CGO_ENABLED=0`，纯 Go sqlite via modernc.org/sqlite）、标准库 `net/http` + `httptest`、bash hook 脚本 + `jq`。无新增第三方依赖。

**Spec:** `docs/superpowers/specs/2026-08-06-memorylake-turn-sync-design.md`（已批准，commit a4952fd）

**Branch:** `feat/memorylake-turn-sync`（已创建，spec 已提交）

## Global Constraints

- **不加 `Co-Authored-By` trailer**（仓库明文规则）。commit 用 conventional commits：`feat(memorylake):` / `test(memorylake):` / `docs(memorylake):`。
- **CI 只跑** `go test ./...` 和 `go test -tags e2e ./internal/server/...`。本计划所有测试都不加 build tag，因此全部由 `go test ./...` 覆盖。无独立 lint 步骤。
- **不得改动任何现有函数签名。** 新能力一律通过新增字段 / 新增方法 / 新增分支实现。
- **未开启本功能的项目行为必须逐字节不变。** 每个任务完成后 `go test ./...` 必须全绿——现有测试就是这条约束的守卫。
- **`ENGRAM_BACKEND=sqlite` 是全局安全阀**，`engram turn` 必须尊重它并立即返回。
- **`engram turn` 永不因运行时问题非零退出**；只有用法错误（缺 `--transcript`、未知 flag）退出码 2。
- **每轮都会跑这个命令**，所以它绝不能做版本更新检查、绝不能打开 SQLite、未开启时绝不发网络请求。
- Go 版本、模块路径以仓库现状为准：import 前缀 `github.com/Gentleman-Programming/engram/`。

---

### Task 1: 开关的配置模型

在 `~/.engram/memorylake.json` 的 `ProjectEntry` 上加一个 bool，并提供读写它的两个方法。这是整个功能的开关底座，也是"旧文件升级后行为不变"这条约束的落点。

**Files:**
- Modify: `internal/memorylake/config.go`（`ProjectEntry` 结构体 + 文件末尾追加两个方法 + import 加 `fmt`）
- Test: `internal/memorylake/config_test.go`（追加三个测试）

**Interfaces:**
- Consumes: 无（本任务是起点）
- Produces:
  - `memorylake.ProjectEntry` 新增字段 `SyncConversations bool`（JSON tag `sync_conversations,omitempty`）
  - `func (e *Enablement) IsConversationSyncEnabled(project string) bool`
  - `func (e *Enablement) SetConversationSync(project string, on bool) error`

- [ ] **Step 1: 写失败的测试**

追加到 `internal/memorylake/config_test.go` 末尾：

```go
func TestSetConversationSyncRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memorylake.json")
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{
		"acme": {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z"},
	}}

	if e.IsConversationSyncEnabled("acme") {
		t.Fatal("conversation sync must default to off")
	}

	if err := e.SetConversationSync("acme", true); err != nil {
		t.Fatalf("SetConversationSync(on): %v", err)
	}
	if err := e.Save(p); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsConversationSyncEnabled("acme") {
		t.Fatal("conversation sync must survive a save/load round trip")
	}
	// Turning it back off must also persist, and must not drop the entry.
	if err := got.SetConversationSync("acme", false); err != nil {
		t.Fatalf("SetConversationSync(off): %v", err)
	}
	if err := got.Save(p); err != nil {
		t.Fatal(err)
	}
	again, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if again.IsConversationSyncEnabled("acme") {
		t.Fatal("conversation sync must be off after disable")
	}
	if entry, ok := again.IsEnabled("acme"); !ok || entry.ProjID != "proj-1" {
		t.Fatalf("disabling conversation sync must not touch backend enablement: %+v ok=%v", entry, ok)
	}
}

// TestSetConversationSyncRequiresBackendEnablement locks decision D1: per-turn
// conversation sync attaches to a project that already routes to MemoryLake.
func TestSetConversationSyncRequiresBackendEnablement(t *testing.T) {
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{}}
	err := e.SetConversationSync("not-enabled", true)
	if err == nil {
		t.Fatal("enabling conversation sync on a non-MemoryLake project must fail")
	}
	if !strings.Contains(err.Error(), "memorylake enable") {
		t.Fatalf("error must tell the user how to fix it, got: %v", err)
	}
	if e.IsConversationSyncEnabled("not-enabled") {
		t.Fatal("a rejected SetConversationSync must not mutate state")
	}
}

// TestLegacyEnablementFileDefaultsConversationSyncOff is the backward-compat
// guard: a memorylake.json written before this feature existed must read back
// with conversation sync off, with no migration step.
func TestLegacyEnablementFileDefaultsConversationSyncOff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memorylake.json")
	legacy := `{"enabled_projects":{"acme":{"proj_id":"proj-1","enabled_at":"2026-07-22T00:00:00Z"}}}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.IsEnabled("acme"); !ok {
		t.Fatal("legacy entry must still be backend-enabled")
	}
	if got.IsConversationSyncEnabled("acme") {
		t.Fatal("legacy entry must have conversation sync off")
	}
}
```

`config_test.go` 当前只 import `path/filepath` 和 `testing`。把 import 块改成：

```go
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/memorylake/ -run 'ConversationSync|LegacyEnablement' -v`
Expected: FAIL —— 编译错误 `e.IsConversationSyncEnabled undefined` / `e.SetConversationSync undefined`

- [ ] **Step 3: 实现**

`internal/memorylake/config.go`，把 `ProjectEntry` 改成：

```go
// ProjectEntry records when (and under which MemoryLake project id) a local
// Engram project was enabled for the MemoryLake backend.
type ProjectEntry struct {
	ProjID    string `json:"proj_id"`
	EnabledAt string `json:"enabled_at"`
	// SyncConversations opts this project into per-turn conversation sync:
	// every completed agent turn is appended to the project's MemoryLake
	// conversation so MemoryLake's own extraction pipeline can mint memories
	// from it (see docs/superpowers/specs/2026-08-06-memorylake-turn-sync-design.md).
	//
	// The zero value is "off", and `omitempty` keeps it out of the serialized
	// file until it is turned on — which is what makes an enablement file
	// written before this field existed read back as "off" with no migration.
	SyncConversations bool `json:"sync_conversations,omitempty"`
}
```

在文件末尾（`IsEnabled` 之后）追加：

```go
// IsConversationSyncEnabled reports whether project has per-turn conversation
// sync turned on. It implies backend enablement: a project absent from
// EnabledProjects is always false.
//
// The lookup is a plain map read on the project name as given, exactly like
// IsEnabled — callers pass a name already produced by project.DetectProject.
// Do not normalize here: a different key convention between this method and
// IsEnabled would make the same project resolve differently in the two, and
// the switch would appear to silently do nothing.
func (e *Enablement) IsConversationSyncEnabled(project string) bool {
	entry, ok := e.EnabledProjects[project]
	return ok && entry.SyncConversations
}

// SetConversationSync turns per-turn conversation sync on or off for project.
// It returns an error when project is not enabled for the MemoryLake backend:
// conversation sync writes into that project's MemoryLake conversation, so it
// has nowhere to go without a resolved MemoryLake project (decision D1).
//
// The caller is responsible for persisting the change via Save.
func (e *Enablement) SetConversationSync(project string, on bool) error {
	entry, ok := e.EnabledProjects[project]
	if !ok {
		return fmt.Errorf("project %q is not enabled for the MemoryLake backend — run `engram memorylake enable --project %s` first", project, project)
	}
	if entry.SyncConversations == on {
		return nil
	}
	entry.SyncConversations = on
	e.EnabledProjects[project] = entry
	return nil
}
```

import 块加 `"fmt"`：

```go
import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/memorylake/ -run 'ConversationSync|LegacyEnablement' -v`
Expected: PASS（3 个测试）

Run: `go test ./...`
Expected: PASS —— 加字段不能破坏任何现有测试

- [ ] **Step 5: 提交**

```bash
git add internal/memorylake/config.go internal/memorylake/config_test.go
git commit -m "feat(memorylake): add per-project conversation-sync switch to enablement file

The switch is a bool on ProjectEntry with omitempty, so an enablement file
written before this field existed reads back as off with no migration step.
SetConversationSync rejects projects that are not MemoryLake-enabled: a
conversation sync has nowhere to write without a resolved MemoryLake project."
```

---

### Task 2: `memorylake conversations` CLI

把 Task 1 的开关暴露成命令，并让 `status` 显示它。做完这一步用户已经能开关这个功能（虽然还没有任何东西读它）。

**Files:**
- Modify: `cmd/engram/main.go`（`cmdMemorylake` 的 switch、新增 `cmdMemorylakeConversations`、`printMemorylakeUsage`、`cmdMemorylakeStatus` 的输出行、`printUsage`）
- Test: `cmd/engram/memorylake_conversations_test.go`（新建）

**Interfaces:**
- Consumes: `memorylake.Enablement.SetConversationSync`、`memorylake.ProjectEntry.SyncConversations`（Task 1）
- Produces: `func cmdMemorylakeConversations()` —— 无参数，从 `os.Args` 读参数，与仓库其他子命令一致

- [ ] **Step 1: 写失败的测试**

新建 `cmd/engram/memorylake_conversations_test.go`：

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

// writeEnablement seeds $HOME/.engram/memorylake.json with the given entries.
// HOME must already be redirected to a temp dir by the caller.
func writeEnablement(t *testing.T, entries map[string]memorylake.ProjectEntry) string {
	t.Helper()
	path := memorylake.DefaultEnablementPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &memorylake.Enablement{EnabledProjects: entries}
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMemorylakeConversationsEnableAndDisable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeEnablement(t, map[string]memorylake.ProjectEntry{
		"acme": {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z"},
	})

	withArgs(t, "engram", "memorylake", "conversations", "enable", "--project", "acme")
	stdout, _ := captureOutput(t, func() { cmdMemorylakeConversations() })
	if !strings.Contains(stdout, "Enabled per-turn conversation sync") {
		t.Fatalf("stdout should confirm the enable, got: %q", stdout)
	}

	enab, err := memorylake.LoadEnablement(path)
	if err != nil {
		t.Fatal(err)
	}
	if !enab.IsConversationSyncEnabled("acme") {
		t.Fatal("enable must persist sync_conversations=true")
	}

	withArgs(t, "engram", "memorylake", "conversations", "disable", "--project", "acme")
	stdout, _ = captureOutput(t, func() { cmdMemorylakeConversations() })
	if !strings.Contains(stdout, "Disabled per-turn conversation sync") {
		t.Fatalf("stdout should confirm the disable, got: %q", stdout)
	}

	enab, err = memorylake.LoadEnablement(path)
	if err != nil {
		t.Fatal(err)
	}
	if enab.IsConversationSyncEnabled("acme") {
		t.Fatal("disable must persist sync_conversations=false")
	}
	if _, ok := enab.IsEnabled("acme"); !ok {
		t.Fatal("disabling conversation sync must not disable the backend")
	}
}

func TestMemorylakeConversationsRejectsUnenabledProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeEnablement(t, map[string]memorylake.ProjectEntry{})

	withArgs(t, "engram", "memorylake", "conversations", "enable", "--project", "ghost")
	_, stderr, code := captureExitPanic(t, func() { cmdMemorylakeConversations() })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "memorylake enable") {
		t.Fatalf("stderr must point at the fix, got: %q", stderr)
	}
}

func TestMemorylakeConversationsRequiresAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	withArgs(t, "engram", "memorylake", "conversations", "--project", "acme")
	_, stderr, code := captureExitPanic(t, func() { cmdMemorylakeConversations() })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "enable|disable") {
		t.Fatalf("stderr must name the valid actions, got: %q", stderr)
	}
}

func TestMemorylakeConversationsRequiresProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	withArgs(t, "engram", "memorylake", "conversations", "enable")
	_, stderr, code := captureExitPanic(t, func() { cmdMemorylakeConversations() })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--project") {
		t.Fatalf("stderr must name the missing flag, got: %q", stderr)
	}
}

func TestMemorylakeStatusShowsConversationSync(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("HOME", t.TempDir())
	writeEnablement(t, map[string]memorylake.ProjectEntry{
		"on-proj":  {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z", SyncConversations: true},
		"off-proj": {ProjID: "proj-2", EnabledAt: "2026-08-06T00:00:00Z"},
	})

	withArgs(t, "engram", "memorylake", "status")
	stdout, _ := captureOutput(t, func() { cmdMemorylakeStatus(cfg) })

	if !strings.Contains(stdout, "conversations=on") {
		t.Fatalf("status must show conversations=on for the enabled project, got: %q", stdout)
	}
	if !strings.Contains(stdout, "conversations=off") {
		t.Fatalf("status must show conversations=off for the other project, got: %q", stdout)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/engram/ -run 'MemorylakeConversations|MemorylakeStatusShowsConversationSync' -v`
Expected: FAIL —— 编译错误 `undefined: cmdMemorylakeConversations`

- [ ] **Step 3: 实现**

`cmd/engram/main.go` 的 `cmdMemorylake` switch 加一个 case（放在 `case "disable":` 之后）：

```go
	case "conversations":
		cmdMemorylakeConversations()
```

`printMemorylakeUsage` 加一行（放在 disable 那行之后）：

```go
	fmt.Fprintln(os.Stderr, "       engram memorylake conversations enable|disable --project <name>")
```

在 `cmdMemorylakeDisable` 之后新增：

```go
// cmdMemorylakeConversations turns per-turn conversation sync on or off for a
// project: with it on, every completed agent turn is appended to the project's
// MemoryLake conversation and MemoryLake extracts memories from it in the
// background (see docs/superpowers/specs/2026-08-06-memorylake-turn-sync-design.md).
//
// Takes no store.Config: this only edits ~/.engram/memorylake.json and never
// touches SQLite.
func cmdMemorylakeConversations() {
	action := ""
	if len(os.Args) > 3 {
		action = os.Args[3]
	}
	if action != "enable" && action != "disable" {
		fmt.Fprintln(os.Stderr, "engram: memorylake conversations requires enable|disable")
		printMemorylakeUsage()
		exitFunc(1)
		return
	}

	project := ""
	for i := 4; i < len(os.Args); i++ {
		if os.Args[i] == "--project" && i+1 < len(os.Args) {
			project = os.Args[i+1]
			i++
		}
	}
	if project == "" {
		fmt.Fprintln(os.Stderr, "engram: --project <name> is required")
		printMemorylakeUsage()
		exitFunc(1)
		return
	}

	path := memorylake.DefaultEnablementPath()
	enablement, err := memorylake.LoadEnablement(path)
	if err != nil {
		fatal(err)
		return
	}
	if err := enablement.SetConversationSync(project, action == "enable"); err != nil {
		fatal(err)
		return
	}
	if err := enablement.Save(path); err != nil {
		fatal(err)
		return
	}

	if action == "enable" {
		fmt.Printf("Enabled per-turn conversation sync for project %q.\n", project)
		fmt.Println("Each completed turn is now appended to this project's MemoryLake conversation; MemoryLake extracts memories from it in the background.")
		return
	}
	fmt.Printf("Disabled per-turn conversation sync for project %q (backend enablement unchanged).\n", project)
}
```

> `fatal` 调用后跟一个裸 `return`：`exitFunc` 在测试里被替换成 panic，但在被替换成"记录退出码后继续"的实现下，没有 `return` 会继续往下走。现有代码里 `cmdMemorylakeEnable` 用的是同样的写法。

`cmdMemorylakeStatus` 里把已启用项目的那一行输出改成：

```go
		if entry, ok := enablement.IsEnabled(name); ok {
			projID := entry.ProjID
			if projID == "" {
				projID = "(pending)"
			}
			convSync := "off"
			if entry.SyncConversations {
				convSync = "on"
			}
			fmt.Printf("  %-30s memorylake  (proj_id=%s, enabled_at=%s, conversations=%s)\n", name, projID, entry.EnabledAt, convSync)
		} else {
```

`printUsage` 里在 `memorylake disable --project <name>` 那两行之后插入：

```
  memorylake conversations enable|disable --project <name>
                     Turn per-turn conversation sync on/off for a project. With it on,
                     every completed agent turn is appended to the project's MemoryLake
                     conversation and MemoryLake extracts memories from it in the
                     background. Requires the project to be memorylake-enabled first.
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/engram/ -run 'MemorylakeConversations|MemorylakeStatusShowsConversationSync' -v`
Expected: PASS（5 个测试）

Run: `go test ./...`
Expected: PASS

> 如果某个既有测试断言了 `memorylake status` 的整行输出，它会因为多了 `conversations=` 而失败。这种情况把该断言一并更新——status 输出格式变更是本任务有意的一部分。

- [ ] **Step 5: 提交**

```bash
git add cmd/engram/main.go cmd/engram/memorylake_conversations_test.go
git commit -m "feat(memorylake): add 'memorylake conversations enable|disable' CLI

Exposes the per-project conversation-sync switch and surfaces its state in
'memorylake status'. Enabling on a project that is not MemoryLake-enabled
fails with a message naming the command that fixes it."
```

---

### Task 3: `internal/turncapture` 的 transcript 解析

新包，纯解析。这是整个功能技术风险最集中的地方：Claude Code transcript 的三个坑（工具回执也是 `type:"user"`、中途插话是 `attachment`、`isMeta` 是注入上下文）全在这里处理。

**Files:**
- Create: `internal/turncapture/turncapture.go`
- Create: `internal/turncapture/turncapture_test.go`

**Interfaces:**
- Consumes: 无（不依赖仓库内任何包）
- Produces:
  - `type Turn struct { SessionID, UserText, AssistantText string }`
  - `func LastTurn(path string) (Turn, error)`

- [ ] **Step 1: 写失败的测试**

新建 `internal/turncapture/turncapture_test.go`：

```go
package turncapture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript writes lines as a JSONL file and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	// A plain human message: content is a bare JSON string.
	lineUserPlain = `{"type":"user","sessionId":"sess-1","isSidechain":false,"message":{"role":"user","content":"add a retry to the uploader"}}`
	// The assistant's final prose reply.
	lineAsstText = `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"Done — the uploader now retries three times."}]}}`
	// Thinking and tool_use must be discarded.
	lineAsstThinking = `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me look at the file"}]}}`
	lineAsstToolUse  = `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{}}]}}`
	// A tool result arrives as type:"user" — it must NOT end the turn.
	lineToolResult = `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
)

func TestLastTurnPlainTextTurn(t *testing.T) {
	p := writeTranscript(t, lineUserPlain, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", turn.SessionID)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q", turn.UserText)
	}
	if turn.AssistantText != "Done — the uploader now retries three times." {
		t.Fatalf("AssistantText = %q", turn.AssistantText)
	}
}

// TestLastTurnSkipsThinkingToolUseAndToolResults is the core boundary test:
// a turn full of tool traffic must still yield exactly the human message and
// the assistant's prose, and the tool_result entries (which are type:"user")
// must not be mistaken for the start of the turn.
func TestLastTurnSkipsThinkingToolUseAndToolResults(t *testing.T) {
	p := writeTranscript(t,
		lineUserPlain,
		lineAsstThinking,
		lineAsstToolUse,
		lineToolResult,
		lineAsstToolUse,
		lineToolResult,
		lineAsstText,
	)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q; tool_result entries must not end the turn", turn.UserText)
	}
	if turn.AssistantText != "Done — the uploader now retries three times." {
		t.Fatalf("AssistantText = %q; thinking/tool_use must be discarded", turn.AssistantText)
	}
}

// TestLastTurnStopsAtTurnBoundary verifies the previous turn does not bleed in.
func TestLastTurnStopsAtTurnBoundary(t *testing.T) {
	prevUser := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"first question"}}`
	prevAsst := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`
	p := writeTranscript(t, prevUser, prevAsst, lineUserPlain, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if strings.Contains(turn.UserText, "first question") {
		t.Fatalf("UserText leaked the previous turn: %q", turn.UserText)
	}
	if strings.Contains(turn.AssistantText, "first answer") {
		t.Fatalf("AssistantText leaked the previous turn: %q", turn.AssistantText)
	}
}

// TestLastTurnCollectsQueuedCommandInterjection covers a mid-turn message the
// user typed while the assistant was working: Claude Code records it as an
// attachment of type queued_command, not as a normal user entry.
func TestLastTurnCollectsQueuedCommandInterjection(t *testing.T) {
	queued := `{"type":"attachment","sessionId":"sess-1","attachment":{"type":"queued_command","prompt":"also bump the timeout"}}`
	// The paired queue-operation entry repeats the same text and must be ignored.
	queueOp := `{"type":"queue-operation","operation":"enqueue","sessionId":"sess-1","content":"also bump the timeout"}`
	p := writeTranscript(t, lineUserPlain, queueOp, queued, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	want := "add a retry to the uploader\n\nalso bump the timeout"
	if turn.UserText != want {
		t.Fatalf("UserText = %q, want %q", turn.UserText, want)
	}
	if strings.Count(turn.UserText, "also bump the timeout") != 1 {
		t.Fatalf("queue-operation must not duplicate the interjection: %q", turn.UserText)
	}
}

// TestLastTurnIgnoresMetaEntries: injected context (skill bodies, system
// prompts) arrives as an isMeta user entry. It is neither captured nor treated
// as a turn boundary.
func TestLastTurnIgnoresMetaEntries(t *testing.T) {
	meta := `{"type":"user","isMeta":true,"sessionId":"sess-1","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /tmp/skill"}]}}`
	p := writeTranscript(t, lineUserPlain, meta, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if strings.Contains(turn.UserText, "Base directory") {
		t.Fatalf("isMeta content must not be captured: %q", turn.UserText)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q", turn.UserText)
	}
}

func TestLastTurnSkipsSidechainEntries(t *testing.T) {
	subUser := `{"type":"user","isSidechain":true,"sessionId":"sess-1","message":{"role":"user","content":"subagent task"}}`
	subAsst := `{"type":"assistant","isSidechain":true,"sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"subagent answer"}]}}`
	p := writeTranscript(t, lineUserPlain, subUser, subAsst, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if strings.Contains(turn.UserText, "subagent") || strings.Contains(turn.AssistantText, "subagent") {
		t.Fatalf("sidechain entries must be skipped: user=%q asst=%q", turn.UserText, turn.AssistantText)
	}
}

func TestLastTurnStripsWrapperTags(t *testing.T) {
	wrapped := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"<system-reminder>be nice</system-reminder>please refactor this<local-command-stdout>ok</local-command-stdout>"}}`
	p := writeTranscript(t, wrapped, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "please refactor this" {
		t.Fatalf("UserText = %q; wrapper tags must be removed whole", turn.UserText)
	}
}

// TestLastTurnBareSlashCommandKeepsCommandName: a slash command with no
// arguments is nothing but wrapper tags. Rather than dropping the turn, the
// command itself stands in as the user's input.
func TestLastTurnBareSlashCommandKeepsCommandName(t *testing.T) {
	slash := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"<command-message>brainstorming</command-message>\n<command-name>superpowers:brainstorming</command-name>"}}`
	p := writeTranscript(t, slash, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "/superpowers:brainstorming" {
		t.Fatalf("UserText = %q, want /superpowers:brainstorming", turn.UserText)
	}
}

func TestLastTurnJoinsMultipleAssistantTextBlocks(t *testing.T) {
	first := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"part one"}]}}`
	second := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"part two"}]}}`
	p := writeTranscript(t, lineUserPlain, first, lineAsstToolUse, second)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.AssistantText != "part one\n\npart two" {
		t.Fatalf("AssistantText = %q, want the blocks joined in order", turn.AssistantText)
	}
}

// TestLastTurnSkipsCorruptLines: the transcript is a live file, so the final
// line can be half-written. A bad line is skipped, not fatal.
func TestLastTurnSkipsCorruptLines(t *testing.T) {
	p := writeTranscript(t, lineUserPlain, lineAsstText, `{"type":"assistant","message":{"content":[{"type":"te`)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("a truncated final line must not fail the parse: %v", err)
	}
	if turn.AssistantText != "Done — the uploader now retries three times." {
		t.Fatalf("AssistantText = %q", turn.AssistantText)
	}
}

func TestLastTurnNoUserMessageYieldsEmptyUserText(t *testing.T) {
	p := writeTranscript(t, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("reaching the top of the file is not an error: %v", err)
	}
	if turn.UserText != "" {
		t.Fatalf("UserText = %q, want empty", turn.UserText)
	}
	if turn.AssistantText == "" {
		t.Fatal("AssistantText should still be captured")
	}
}

func TestLastTurnMissingFileIsError(t *testing.T) {
	if _, err := LastTurn(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("an unreadable transcript must be an error")
	}
}

// TestLastTurnTailWindowOnHugeTranscript exercises the large-file path: with a
// 1-byte whole-file ceiling the tail window is used, its first (possibly
// truncated) line is dropped, and the turn is still found inside the window.
func TestLastTurnTailWindowOnHugeTranscript(t *testing.T) {
	t.Setenv("ENGRAM_TURN_MAX_TRANSCRIPT_BYTES", "1")
	t.Setenv("ENGRAM_TURN_TAIL_WINDOW_BYTES", "4096")

	filler := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}`
	p := writeTranscript(t, filler, lineUserPlain, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q", turn.UserText)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/turncapture/ -v`
Expected: FAIL —— `no Go files in .../internal/turncapture`（或 `undefined: LastTurn`）

- [ ] **Step 3: 实现**

新建 `internal/turncapture/turncapture.go`：

```go
// Package turncapture extracts the last completed conversational turn — one
// human message plus the assistant's final reply — from an AI coding agent's
// session transcript, and renders it as a single text blob suitable for
// appending to a MemoryLake conversation.
//
// The Turn type is agent-agnostic; only the parser is Claude-Code-specific.
// Adding a Codex or OpenCode transcript format later means adding one file
// here and touching nothing else in the tree.
//
// Deliberately dependency-free: no network, no store, no MemoryLake concepts.
package turncapture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Turn is one round of conversation in the form it gets written to MemoryLake.
// A Turn with an empty UserText or AssistantText must not be written — see
// Merged.
type Turn struct {
	SessionID     string
	UserText      string
	AssistantText string
}

// entry is the subset of a Claude Code transcript JSONL line this package
// reads. Every other field in the line is ignored.
type entry struct {
	Type        string `json:"type"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	SessionID   string `json:"sessionId"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
	} `json:"attachment"`
}

// block is one element of a message's content array.
type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// LastTurn parses the Claude Code transcript at path and returns its final
// turn.
//
// Error contract, deliberately narrow because the caller is a fire-and-forget
// hook: an unreadable path is an error; a malformed JSONL line is skipped
// silently (the transcript is a live file — Claude Code may be mid-write when
// the Stop hook fires); and reaching the top of the file without finding a
// human message returns a Turn with an empty UserText and a nil error, leaving
// the "is this worth writing" decision to Merged.
func LastTurn(path string) (Turn, error) {
	lines, err := readLines(path)
	if err != nil {
		return Turn{}, fmt.Errorf("turncapture: read transcript: %w", err)
	}

	var t Turn
	var userParts, assistantParts []string

	// Scan backwards from the end of the file: the turn we want is the tail.
	// Each captured fragment is prepended, so the parts come out in
	// chronological order even though the walk is reversed.
scan:
	for i := len(lines) - 1; i >= 0; i-- {
		var e entry
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			continue
		}
		if e.IsSidechain {
			continue
		}
		// The last line carrying a session id wins, which makes this the
		// session the transcript itself claims to belong to.
		if t.SessionID == "" && e.SessionID != "" {
			t.SessionID = e.SessionID
		}

		switch {
		case e.Type == "assistant":
			if txt := textBlocks(e.Message.Content); txt != "" {
				assistantParts = append([]string{txt}, assistantParts...)
			}

		case e.Type == "attachment" && e.Attachment.Type == "queued_command":
			// A message the user typed while the assistant was still working.
			// Part of this turn's input, but not its starting boundary.
			if p := strings.TrimSpace(e.Attachment.Prompt); p != "" {
				userParts = append([]string{p}, userParts...)
			}

		case e.Type == "user":
			// Tool results are recorded as type:"user" too. Treating them as
			// the turn boundary would clip every turn at its last tool call.
			if hasBlockType(e.Message.Content, "tool_result") {
				continue
			}
			// isMeta entries are injected context (skill bodies, system
			// prompts), not something the human typed.
			if e.IsMeta {
				continue
			}
			if txt := cleanUserText(rawText(e.Message.Content)); txt != "" {
				userParts = append([]string{txt}, userParts...)
			}
			// A real human message starts this turn — stop here.
			break scan
		}
	}

	t.UserText = strings.Join(compact(userParts), "\n\n")
	t.AssistantText = strings.Join(compact(assistantParts), "\n\n")
	return t, nil
}

// readLines reads path into lines. Whole-file for anything up to
// ENGRAM_TURN_MAX_TRANSCRIPT_BYTES; beyond that only the final
// ENGRAM_TURN_TAIL_WINDOW_BYTES are read and the window's first line is
// dropped because the window boundary almost certainly cut it in half.
//
// Uses bufio.Reader rather than bufio.Scanner on purpose: a single transcript
// line (a large tool_result, a whole file's contents) routinely exceeds
// Scanner's token limit, and Scanner's response to that is to abort the whole
// read rather than skip the line.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	dropFirst := false
	if maxWhole := int64(envInt("ENGRAM_TURN_MAX_TRANSCRIPT_BYTES", 64<<20)); st.Size() > maxWhole {
		window := int64(envInt("ENGRAM_TURN_TAIL_WINDOW_BYTES", 2<<20))
		if window > st.Size() {
			window = st.Size()
		}
		if _, err := f.Seek(st.Size()-window, io.SeekStart); err != nil {
			return nil, err
		}
		dropFirst = true
	}

	br := bufio.NewReaderSize(f, 256*1024)
	var lines []string
	for {
		line, readErr := br.ReadString('\n')
		if line != "" {
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, readErr
		}
	}
	if dropFirst && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines, nil
}

// envInt reads a positive integer from the environment, falling back to def for
// an unset, non-numeric, or non-positive value. Mirrors the tolerant behavior of
// internal/memorylake's envInt.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// rawText renders a message's content as plain text. Content is either a bare
// JSON string (the common shape for a typed message) or an array of blocks.
func rawText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return textBlocks(raw)
}

// textBlocks joins every "text" block of a content array, discarding thinking,
// tool_use, tool_result and image blocks. A non-array content yields "".
func textBlocks(raw json.RawMessage) string {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if txt := strings.TrimSpace(b.Text); txt != "" {
			parts = append(parts, txt)
		}
	}
	return strings.Join(parts, "\n\n")
}

// hasBlockType reports whether a content array contains a block of type want.
func hasBlockType(raw json.RawMessage, want string) bool {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == want {
			return true
		}
	}
	return false
}

// wrapperTags are the XML-ish envelopes Claude Code puts around injected
// content inside an otherwise human-authored message. Their contents are not
// what the user typed, so they are removed whole rather than kept as noise.
var wrapperTags = []string{
	"command-message",
	"command-name",
	"system-reminder",
	"local-command-stdout",
}

// cleanUserText removes every wrapper tag block from s. When that leaves
// nothing but s did carry a <command-name>, the slash command itself stands in
// (e.g. "/superpowers:brainstorming") so a bare slash-command turn stays
// attributable instead of being dropped entirely.
func cleanUserText(s string) string {
	commandName := strings.TrimSpace(innerText(s, "command-name"))

	out := s
	for _, tag := range wrapperTags {
		out = stripTag(out, tag)
	}
	out = strings.TrimSpace(out)

	if out == "" && commandName != "" {
		return "/" + strings.TrimPrefix(commandName, "/")
	}
	return out
}

// stripTag removes every <tag>…</tag> block, contents included, from s. An
// unterminated opener drops everything from the opener onward rather than
// leaving a dangling marker in the captured text.
func stripTag(s, tag string) string {
	open, closing := "<"+tag+">", "</"+tag+">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		rest := s[i+len(open):]
		j := strings.Index(rest, closing)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + rest[j+len(closing):]
	}
}

// innerText returns the contents of the first <tag>…</tag> block in s, or "".
func innerText(s, tag string) string {
	open, closing := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// compact trims each part and drops the ones that end up empty.
func compact(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/turncapture/ -v`
Expected: PASS（14 个测试）

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/turncapture/
git commit -m "feat(turncapture): parse the last turn out of a Claude Code transcript

Backwards scan over the JSONL with three non-obvious rules the real format
forces: tool results arrive as type:\"user\" and must not end the turn,
mid-turn interjections are queued_command attachments rather than user
entries, and isMeta user entries are injected context, not human input.
Malformed lines are skipped because the transcript is a live file."
```

---

### Task 4: `Turn.Merged` —— 渲染与截断

把一个 `Turn` 变成要写入的那条消息，包含"太大就截断"和"不完整就不写"两个判断。放在 `turncapture` 而不是 `cmd/engram`：纯字符串运算，和解析共享同一套 fixture。

**Files:**
- Create: `internal/turncapture/merge.go`
- Create: `internal/turncapture/merge_test.go`

**Interfaces:**
- Consumes: `turncapture.Turn`（Task 3）
- Produces: `func (t Turn) Merged(maxBytes int) (content string, ok bool)` —— `ok == false` 表示该轮不应写入

- [ ] **Step 1: 写失败的测试**

新建 `internal/turncapture/merge_test.go`：

```go
package turncapture

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMergedRendersBothSpeakers(t *testing.T) {
	turn := Turn{UserText: "fix the uploader", AssistantText: "done"}

	got, ok := turn.Merged(32768)
	if !ok {
		t.Fatal("a complete turn must be writable")
	}
	want := "**User:**\nfix the uploader\n\n**Assistant:**\ndone"
	if got != want {
		t.Fatalf("Merged = %q, want %q", got, want)
	}
}

func TestMergedRejectsIncompleteTurns(t *testing.T) {
	cases := []struct {
		name string
		turn Turn
	}{
		{"no user text", Turn{AssistantText: "done"}},
		{"no assistant text", Turn{UserText: "fix it"}},
		{"whitespace only", Turn{UserText: "   ", AssistantText: "\n\t"}},
		{"both empty", Turn{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.turn.Merged(32768); ok {
				t.Fatal("an incomplete turn must not be writable")
			}
		})
	}
}

func TestMergedExactlyAtLimitIsNotTruncated(t *testing.T) {
	turn := Turn{UserText: "abc", AssistantText: "de"}
	full, ok := turn.Merged(0) // 0 disables the ceiling
	if !ok {
		t.Fatal("want writable")
	}

	got, ok := turn.Merged(len(full))
	if !ok {
		t.Fatal("want writable")
	}
	if got != full {
		t.Fatalf("content exactly at the limit must be untouched, got %q", got)
	}
}

func TestMergedTruncatesOverLimitKeepingHeadAndTail(t *testing.T) {
	user := strings.Repeat("u", 20000)
	asst := strings.Repeat("a", 20000)
	turn := Turn{UserText: user, AssistantText: asst}

	got, ok := turn.Merged(8192)
	if !ok {
		t.Fatal("an over-long turn must still be writable, just truncated")
	}
	if len(got) > 8192 {
		t.Fatalf("len = %d, must not exceed the 8192 ceiling", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncation must be marked in the content: %q", got[:200])
	}
	// Both ends of both parts must survive.
	if !strings.Contains(got, "**User:**\nuuu") {
		t.Fatal("the head of the user text must survive")
	}
	if !strings.HasSuffix(got, "a") {
		t.Fatal("the tail of the assistant text must survive")
	}
}

func TestMergedTruncationKeepsValidUTF8(t *testing.T) {
	// Multibyte runes throughout, so a naive byte cut lands mid-rune.
	user := strings.Repeat("用户说的话", 2000)
	asst := strings.Repeat("助手的回复", 2000)
	turn := Turn{UserText: user, AssistantText: asst}

	got, ok := turn.Merged(4096)
	if !ok {
		t.Fatal("want writable")
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation must not split a multibyte rune")
	}
	if len(got) > 4096 {
		t.Fatalf("len = %d, must not exceed the ceiling", len(got))
	}
}

// TestMergedRejectsUnworkableBudget: with a ceiling too small to give both
// parts their floor, the whole turn is dropped rather than written as a stub.
func TestMergedRejectsUnworkableBudget(t *testing.T) {
	turn := Turn{
		UserText:      strings.Repeat("u", 5000),
		AssistantText: strings.Repeat("a", 5000),
	}
	if _, ok := turn.Merged(512); ok {
		t.Fatal("a budget below the two-part floor must drop the turn")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/turncapture/ -run Merged -v`
Expected: FAIL —— `turn.Merged undefined`

- [ ] **Step 3: 实现**

新建 `internal/turncapture/merge.go`：

```go
package turncapture

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	userHeader      = "**User:**\n"
	assistantHeader = "\n\n**Assistant:**\n"

	// minPartBudget is the floor each half of a turn gets before truncation is
	// considered pointless. Below this a "turn" is too mutilated to extract
	// anything useful from, so it is dropped instead of written.
	minPartBudget = 1024
)

// Merged renders t as the single message body written to a MemoryLake
// conversation:
//
//	**User:**
//	<user text>
//
//	**Assistant:**
//	<assistant text>
//
// The speaker labels live in the text because MemoryLake derives a message's
// role from its actor's type and only HUMAN actors can be created through the
// API — so both halves are posted as the same HUMAN actor and the role
// information has to survive in the prose (design decision D3).
//
// ok is false when the turn must not be written: either half is empty, or
// maxBytes is too small to leave both halves their floor. maxBytes <= 0
// disables the ceiling.
func (t Turn) Merged(maxBytes int) (string, bool) {
	user := strings.TrimSpace(t.UserText)
	assistant := strings.TrimSpace(t.AssistantText)
	if user == "" || assistant == "" {
		return "", false
	}

	full := userHeader + user + assistantHeader + assistant
	if maxBytes <= 0 || len(full) <= maxBytes {
		return full, true
	}

	budget := maxBytes - len(userHeader) - len(assistantHeader)
	if budget < 2*minPartBudget {
		return "", false
	}

	// Split the budget in proportion to the original sizes, then hold each
	// half to its floor.
	userBudget := budget * len(user) / (len(user) + len(assistant))
	if userBudget < minPartBudget {
		userBudget = minPartBudget
	}
	assistantBudget := budget - userBudget
	if assistantBudget < minPartBudget {
		assistantBudget = minPartBudget
		userBudget = budget - assistantBudget
	}

	return userHeader + truncateMiddle(user, userBudget) +
		assistantHeader + truncateMiddle(assistant, assistantBudget), true
}

// truncateMiddle shortens s to at most budget bytes by keeping its head (60%)
// and tail (40%) with a marker in between. Head-and-tail rather than head-only
// because a turn's conclusion usually sits at the end — the most valuable part
// is exactly what a head-only cut would throw away.
//
// Both cuts land on rune boundaries, so the result is always valid UTF-8.
func truncateMiddle(s string, budget int) string {
	if len(s) <= budget {
		return s
	}

	marker := fmt.Sprintf("\n…[truncated %d bytes]…\n", len(s)-budget)
	if len(marker) >= budget {
		// No room for both a marker and content: keep a head-only slice.
		return trimTrailingPartialRune(s[:budget])
	}

	keep := budget - len(marker)
	head := keep * 6 / 10
	tail := keep - head
	return trimTrailingPartialRune(s[:head]) + marker + trimLeadingPartialRune(s[len(s)-tail:])
}

// trimTrailingPartialRune drops an incomplete UTF-8 sequence from the end of s.
// Bounded to 3 steps (the longest possible partial sequence) so a string that
// legitimately contains U+FFFD is never eaten.
func trimTrailingPartialRune(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// trimLeadingPartialRune drops an incomplete UTF-8 sequence from the start of s.
func trimLeadingPartialRune(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		if r, size := utf8.DecodeRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[1:]
	}
	return s
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/turncapture/ -v`
Expected: PASS（Task 3 的 14 个 + 本任务的 6 个）

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/turncapture/merge.go internal/turncapture/merge_test.go
git commit -m "feat(turncapture): render a turn into one merged message with truncation

Speaker labels go in the prose because MemoryLake derives a message role from
its actor type and the API can only mint HUMAN actors. Over-long turns keep
head and tail rather than just the head — a turn's conclusion sits at the end.
Cuts land on rune boundaries; a budget below both halves' floor drops the turn."
```

---

### Task 5: `AppendTurn` 写入路径

MemoryLake 侧的写入方法。薄到几乎只是转发，但要独立测：断言"恰好两次请求"和"内容幂等"。

**Files:**
- Create: `internal/memorylake/turns.go`
- Create: `internal/memorylake/turns_test.go`

**Interfaces:**
- Consumes: 既有的 `(*Client).AppendObservation`、`defaultConversationCustomID`、`contentHash`
- Produces: `func (b *MemoryLakeBackend) AppendTurn(sessionID, content string) (string, error)`

- [ ] **Step 1: 写失败的测试**

新建 `internal/memorylake/turns_test.go`：

```go
package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// turnServer answers the two calls AppendTurn makes and records what arrived.
func turnServer(t *testing.T, convPosts, msgPosts *int32, gotText *string, gotConvCustomID *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			atomic.AddInt32(convPosts, 1)
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			*gotConvCustomID = body.CustomID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})

		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(msgPosts, 1)
			var body struct {
				CustomID string `json:"custom_id"`
				Content  []struct {
					BlockType string `json:"block_type"`
					Text      string `json:"text"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Content) > 0 {
				*gotText = body.Content[0].Text
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-" + body.CustomID}})

		default:
			t.Fatalf("unexpected request %s %s (AppendTurn must only ensure a conversation and append one message)", r.Method, r.URL.Path)
		}
	}))
}

func TestAppendTurnPostsOneMessageOnTheSessionConversation(t *testing.T) {
	var convPosts, msgPosts int32
	var gotText, gotConvID string
	srv := turnServer(t, &convPosts, &msgPosts, &gotText, &gotConvID)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	content := "**User:**\nfix the uploader\n\n**Assistant:**\ndone"
	id, err := b.AppendTurn("sess-42", content)
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if id == "" {
		t.Fatal("AppendTurn must return the MemoryLake message id")
	}
	if convPosts != 1 || msgPosts != 1 {
		t.Fatalf("convPosts=%d msgPosts=%d, want 1/1", convPosts, msgPosts)
	}
	if gotConvID != "sess-42" {
		t.Fatalf("conversation custom_id = %q, want the session id", gotConvID)
	}
	if gotText != content {
		t.Fatalf("posted text = %q, want the merged content verbatim", gotText)
	}
}

// TestAppendTurnIsIdempotentOnContent locks the re-run guarantee: the message
// custom_id is a content hash, so replaying the same turn maps to the same
// message id rather than creating a second one.
func TestAppendTurnIsIdempotentOnContent(t *testing.T) {
	var convPosts, msgPosts int32
	var gotText, gotConvID string
	srv := turnServer(t, &convPosts, &msgPosts, &gotText, &gotConvID)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	content := "**User:**\nsame\n\n**Assistant:**\nsame"
	first, err := b.AppendTurn("sess-1", content)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.AppendTurn("sess-1", content)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same content must map to the same message id: %q vs %q", first, second)
	}
}

func TestAppendTurnWithoutSessionUsesDefaultConversation(t *testing.T) {
	var convPosts, msgPosts int32
	var gotText, gotConvID string
	srv := turnServer(t, &convPosts, &msgPosts, &gotText, &gotConvID)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AppendTurn("", "**User:**\nx\n\n**Assistant:**\ny"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if gotConvID != defaultConversationCustomID {
		t.Fatalf("conversation custom_id = %q, want %q", gotConvID, defaultConversationCustomID)
	}
}

func TestAppendTurnRejectsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("empty content must not reach the network (%s %s)", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AppendTurn("sess-1", "   \n\t "); err == nil {
		t.Fatal("empty content must be rejected")
	}
}

// TestAppendTurnRecoversFromConversationConflict covers the normal case for
// every turn after the first in a session: the create call rejects the
// duplicate custom_id and the existing conversation is fetched instead.
func TestAppendTurnRecoversFromConversationConflict(t *testing.T) {
	var msgPosts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			w.WriteHeader(http.StatusConflict)
			// Error shape copied from internal/memorylake/identity_test.go:
			// error_code is top-level, not nested under an "error" object.
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "custom_id already exists", "error_code": "CUSTOM_ID_CONFLICT",
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/memories/conversations/"):
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-existing"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-existing/messages":
			atomic.AddInt32(&msgPosts, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AppendTurn("sess-1", "**User:**\nx\n\n**Assistant:**\ny"); err != nil {
		t.Fatalf("AppendTurn must recover from CUSTOM_ID_CONFLICT: %v", err)
	}
	if msgPosts != 1 {
		t.Fatalf("msgPosts=%d, want 1 on the existing conversation", msgPosts)
	}
}
```

> 冲突响应体的形状取自 `internal/memorylake/identity_test.go:425`：`error_code` 是**顶层**字段（不是嵌套在 `error` 对象里），`Client.doJSON` 据此产出 `*APIError{Code: "CUSTOM_ID_CONFLICT"}`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/memorylake/ -run AppendTurn -v`
Expected: FAIL —— `b.AppendTurn undefined`

- [ ] **Step 3: 实现**

新建 `internal/memorylake/turns.go`：

```go
package memorylake

import (
	"fmt"
	"strings"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// AppendTurn appends one completed agent turn — already rendered by
// turncapture.Turn.Merged — as a single message on the MemoryLake conversation
// keyed by sessionID, and returns the MemoryLake message id.
//
// It rides the same path AddPrompt uses (see writequeue.go's
// AppendObservation): ensure the conversation exists by custom_id, then append
// one message whose own custom_id is a hash of the content. That hash is what
// makes replaying a turn safe — MemoryLake resolves the duplicate custom_id to
// the message it already has instead of creating a second one, so re-running
// `engram turn` over the same transcript is a no-op.
//
// Like AddObservation, this does not wait for MemoryLake's extraction pipeline
// and never backfills its result: any fact minted from this turn shows up later
// through the normal read paths (Search, Timeline, FormatContext).
//
// An empty (or whitespace-only) content is rejected rather than posted: a blank
// message would consume an extraction slot and produce nothing.
func (b *MemoryLakeBackend) AppendTurn(sessionID, content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("memorylake: AppendTurn: content is empty")
	}

	convCustomID := sessionID
	if convCustomID == "" {
		convCustomID = defaultConversationCustomID
	}

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	// Type/Title are not part of the conversation-append request body (only
	// Content is) — they are set so logs and future debugging can tell what
	// this message was.
	return b.client.AppendObservation(b.ws, b.projID, convCustomID, b.actorID, store.AddObservationParams{
		SessionID: sessionID,
		Type:      "turn",
		Title:     "Conversation turn",
		Content:   content,
	})
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/memorylake/ -run AppendTurn -v`
Expected: PASS（5 个测试）

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/memorylake/turns.go internal/memorylake/turns_test.go
git commit -m "feat(memorylake): add AppendTurn to write one turn as a conversation message

Reuses AppendObservation's ensure-conversation-then-append path, so the
message custom_id is a content hash and replaying the same turn resolves to
the message MemoryLake already has instead of duplicating it."
```

---

### Task 6: prompt 追加抑制

开关打开时不要让用户提问在云端出现两次。这是"开关打开后行为正确"和"开关关闭后行为不变"之间的接缝，测试里那条"零 HTTP 请求"的断言是唯一能防止后续改动悄悄破坏它的东西。

**Files:**
- Modify: `internal/memorylake/backend.go`（`MemoryLakeBackend` 加字段 + 加 setter）
- Modify: `internal/memorylake/prompts.go`（`appendPrompt` 加一个前置分支）
- Modify: `cmd/engram/routing.go`（`resolveMemoryLakeBackend` 构造后接线）
- Test: `internal/memorylake/prompts_test.go`（追加两个测试）

**Interfaces:**
- Consumes: `memorylake.ProjectEntry.SyncConversations`（Task 1）
- Produces: `func (b *MemoryLakeBackend) SetSkipPromptAppend(v bool)`

- [ ] **Step 1: 写失败的测试**

追加到 `internal/memorylake/prompts_test.go` 末尾：

```go
// TestBackend_SkipPromptAppend_MakesNoRequest is the regression lock for the
// "not affecting existing behavior" half of per-turn conversation sync: with
// the flag set, prompt persistence must not touch the network at all (the
// merged turn message already carries the user's text), yet must still hand
// callers a stable, non-empty id so no call site needs a special case.
func TestBackend_SkipPromptAppend_MakesNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no request may be made while prompt append is suppressed (%s %s)", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)
	b.SetSkipPromptAppend(true)

	p := store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "please fix the bug"}

	id, err := b.AddPrompt(p)
	if err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if id == "" {
		t.Fatal("AddPrompt must still return a stable non-empty id")
	}

	id2, inserted, err := b.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("AddPromptIfMissing: %v", err)
	}
	if !inserted {
		t.Fatal("first AddPromptIfMissing must still report inserted=true")
	}
	if id2 != id {
		t.Fatalf("suppressed ids must be stable for identical content: %q vs %q", id2, id)
	}

	_, inserted2, err := b.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("AddPromptIfMissing (repeat): %v", err)
	}
	if inserted2 {
		t.Fatal("repeat AddPromptIfMissing must still report inserted=false")
	}
}

// TestBackend_SkipPromptAppend_DefaultsOff proves the flag is opt-in: a backend
// nobody configured behaves exactly as before.
func TestBackend_SkipPromptAppend_DefaultsOff(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AddPrompt(store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "hello"}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts=%d, want 1 — suppression must default to off", posts)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/memorylake/ -run SkipPromptAppend -v`
Expected: FAIL —— `b.SetSkipPromptAppend undefined`

- [ ] **Step 3: 实现**

`internal/memorylake/backend.go`，在 `MemoryLakeBackend` 结构体末尾（`passiveSeen` 之后）加字段：

```go
	// skipPromptAppend suppresses AddPrompt/AddPromptIfMissing's conversation
	// append. It is set for projects with per-turn conversation sync enabled:
	// the merged turn message already contains the user's prompt verbatim, so
	// appending it a second time on its own would put the same sentence in the
	// conversation twice and skew MemoryLake's extraction.
	//
	// Suppressing it costs nothing locally because prompts are write-only on
	// this backend — no read path reads them back (FormatContext, Stats and
	// Timeline all have no prompt section; see CreateSession/AddPrompt's doc
	// comments). Not guarded by a mutex: it is set once by the routing layer
	// immediately after construction, before the backend reaches any handler.
	skipPromptAppend bool
```

在 `NewBackend` 之后加 setter：

```go
// SetSkipPromptAppend toggles prompt-append suppression — see the
// skipPromptAppend field comment for why per-turn conversation sync needs it.
// Call it right after NewBackend, before the backend is handed to any handler.
func (b *MemoryLakeBackend) SetSkipPromptAppend(v bool) {
	b.skipPromptAppend = v
}
```

`internal/memorylake/prompts.go` 的 `appendPrompt` 开头插入：

```go
func (b *MemoryLakeBackend) appendPrompt(p store.AddPromptParams) (string, error) {
	if b.skipPromptAppend {
		// Per-turn conversation sync owns this content now (see
		// skipPromptAppend). Return the same stable, non-empty id shape a real
		// append would have produced so no caller needs a special case.
		return contentHash(p.Content), nil
	}

	convCustomID := p.SessionID
	// … 其余不变
```

`cmd/engram/routing.go` 的 `resolveMemoryLakeBackend` 末尾，把

```go
	backend, err := memorylake.NewBackend(cfg, ws, projID)
	if err != nil {
		warnMemoryLakeFallback(project, "constructing backend", err)
		return sqlite, false
	}
	return backend, true
```

改成

```go
	backend, err := memorylake.NewBackend(cfg, ws, projID)
	if err != nil {
		warnMemoryLakeFallback(project, "constructing backend", err)
		return sqlite, false
	}
	// With per-turn conversation sync on, the Stop hook's `engram turn` already
	// writes the user's prompt as part of the merged turn message — appending
	// it a second time here would duplicate it in the conversation.
	backend.SetSkipPromptAppend(entry.SyncConversations)
	return backend, true
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/memorylake/ -run 'Prompt' -v`
Expected: PASS（既有 prompt 测试 + 新增 2 个）

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/memorylake/backend.go internal/memorylake/prompts.go internal/memorylake/prompts_test.go cmd/engram/routing.go
git commit -m "feat(memorylake): suppress prompt append when conversation sync is on

The merged turn message already carries the user's prompt verbatim, so the
standalone prompt append would put the same sentence in the conversation
twice and skew extraction. Safe to drop because prompts are write-only on
this backend. Defaults off, so projects without the switch are untouched."
```

---

### Task 7: `engram turn` 子命令

把前面所有零件接成一个命令。这个任务里三处"每轮都会跑"的性能/安全约束比功能本身更容易被忽略：不做版本更新检查、不打开 SQLite、未开启时不发请求。

**Files:**
- Create: `cmd/engram/turn.go`
- Create: `cmd/engram/turn_test.go`
- Modify: `cmd/engram/main.go`（`shouldCheckForUpdates`、`handleConfigFreeCommand`、`main` 的 switch、`printUsage`）

**Interfaces:**
- Consumes: `turncapture.LastTurn`、`turncapture.Turn.Merged`（Task 3/4）、`memorylake.AppendTurn`（Task 5）、`memorylake.Enablement`（Task 1）、既有的 `detectProject` / `loadMemorylakeConfig` / `loadMemorylakeEnablement` / `exitFunc` 变量
- Produces: `func cmdTurn()`

- [ ] **Step 1: 写失败的测试**

新建 `cmd/engram/turn_test.go`：

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

// failingMemoryLake is a server that fails the test on any request. Used to
// prove the "no network" claims.
func failingMemoryLake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no MemoryLake request may be made (%s %s)", r.Method, r.URL.Path)
	}))
}

// writeTurnTranscript writes a minimal one-turn transcript and returns its path.
func writeTurnTranscript(t *testing.T, sessionID string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := []string{
		`{"type":"user","sessionId":"` + sessionID + `","message":{"role":"user","content":"fix the uploader"}}`,
		`{"type":"assistant","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCmdTurnUnenabledProjectMakesNoNetworkCall is the hot path: almost every
// invocation of `engram turn` lands here and must cost nothing.
func TestCmdTurnUnenabledProjectMakesNoNetworkCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	transcript := writeTurnTranscript(t, "sess-1")
	withArgs(t, "engram", "turn", "--session", "sess-1", "--transcript", transcript, "--cwd", t.TempDir())

	captureOutput(t, func() { cmdTurn() })
}

// TestCmdTurnRespectsSqliteSafetyValve: ENGRAM_BACKEND=sqlite disables the
// MemoryLake path globally, and per-turn sync must honor it too.
func TestCmdTurnRespectsSqliteSafetyValve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ENGRAM_BACKEND", "sqlite")
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	// Fully enabled — only the safety valve should stop it.
	seedTurnEnablement(t, "acme", true)
	transcript := writeTurnTranscript(t, "sess-1")
	withArgs(t, "engram", "turn", "--session", "sess-1", "--transcript", transcript, "--cwd", turnProjectDir(t, "acme"))

	captureOutput(t, func() { cmdTurn() })
}

func TestCmdTurnMissingTranscriptExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withArgs(t, "engram", "turn", "--session", "sess-1")

	_, stderr, code := captureExitPanic(t, func() { cmdTurn() })
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a usage error", code)
	}
	if !strings.Contains(stderr, "--transcript") {
		t.Fatalf("stderr must name the missing flag, got %q", stderr)
	}
}

func TestCmdTurnUnknownFlagExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withArgs(t, "engram", "turn", "--transcirpt", "/tmp/x")

	_, stderr, code := captureExitPanic(t, func() { cmdTurn() })
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for an unknown flag", code)
	}
	if !strings.Contains(stderr, "--transcirpt") {
		t.Fatalf("stderr must echo the bad flag, got %q", stderr)
	}
}

// TestCmdTurnMissingTranscriptFileExitsZero: a runtime problem is never a
// non-zero exit — the hook must not surface anything to the user.
func TestCmdTurnMissingTranscriptFileExitsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTurnEnablement(t, "acme", true)
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	withArgs(t, "engram", "turn",
		"--session", "sess-1",
		"--transcript", filepath.Join(t.TempDir(), "does-not-exist.jsonl"),
		"--cwd", turnProjectDir(t, "acme"))

	captureOutput(t, func() { cmdTurn() })

	// The failure must be recorded in the log file, not the terminal.
	logPath := filepath.Join(home, ".engram", "logs", "turn.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("a parse failure must be logged to %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "session=sess-1") {
		t.Fatalf("log line must identify the session, got %q", data)
	}
}

func TestCmdTurnAppendsTurnForEnabledProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTurnEnablement(t, "acme", true)

	var msgPosts int32
	var gotText, gotConvCustomID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// NewBackend: EnsureActor create + workspace binding.
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		// AppendTurn.
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotConvCustomID = body.CustomID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&msgPosts, 1)
			var body struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Content) > 0 {
				gotText = body.Content[0].Text
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")
	// A "ws-" prefixed workspace short-circuits ResolveWorkspaceID, so the
	// test server does not need a workspace list endpoint.
	t.Setenv("ENGRAM_MEMORYLAKE_WORKSPACE", "ws-1")

	transcript := writeTurnTranscript(t, "sess-9")
	withArgs(t, "engram", "turn",
		"--session", "sess-9",
		"--transcript", transcript,
		"--cwd", turnProjectDir(t, "acme"),
		"--verbose")

	stdout, _ := captureOutput(t, func() { cmdTurn() })

	if msgPosts != 1 {
		t.Fatalf("msgPosts = %d, want exactly 1", msgPosts)
	}
	if gotConvCustomID != "sess-9" {
		t.Fatalf("conversation custom_id = %q, want sess-9", gotConvCustomID)
	}
	if !strings.Contains(gotText, "**User:**") || !strings.Contains(gotText, "fix the uploader") {
		t.Fatalf("posted text must be the merged turn, got %q", gotText)
	}
	if !strings.Contains(gotText, "**Assistant:**") || !strings.Contains(gotText, "done") {
		t.Fatalf("posted text must include the assistant reply, got %q", gotText)
	}
	if !strings.Contains(stdout, "appended turn to conversation sess-9") {
		t.Fatalf("--verbose must report the append, got %q", stdout)
	}
}

// TestCmdTurnEnabledBackendButConversationSyncOffMakesNoCall covers the
// half-enabled state: MemoryLake backend on, per-turn sync off.
func TestCmdTurnEnabledBackendButConversationSyncOffMakesNoCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTurnEnablement(t, "acme", false)
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	transcript := writeTurnTranscript(t, "sess-1")
	withArgs(t, "engram", "turn", "--session", "sess-1", "--transcript", transcript, "--cwd", turnProjectDir(t, "acme"))

	captureOutput(t, func() { cmdTurn() })
}

// seedTurnEnablement writes $HOME/.engram/memorylake.json with project enabled
// for the MemoryLake backend and conversation sync set to syncOn.
func seedTurnEnablement(t *testing.T, project string, syncOn bool) {
	t.Helper()
	path := memorylake.DefaultEnablementPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		project: {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z", SyncConversations: syncOn},
	}}
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
}

// turnProjectDir returns a directory whose detected project name is `project`,
// by writing an .engram/config that names it explicitly. This keeps the test
// independent of git remotes and of the directory's own basename.
func turnProjectDir(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".engram")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// internal/project/detect.go's readConfigAt reads .engram/config.json and
	// unmarshals it into configFile{ProjectName string `json:"project_name"`}.
	body := []byte(`{"project_name":"` + project + `"}`)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
```

> 退路：如果 `.engram/config.json` 的探测在非 git 目录下不生效（`detectFromConfig` 会向上/向下找最近的配置，停点与 git root 有关），改用目录 basename 作为项目名——把每个测试里的 `seedTurnEnablement(t, "acme", …)` 改成先取 `dir := turnProjectDir(t, "")` 再 `seedTurnEnablement(t, filepath.Base(dir), …)`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/engram/ -run CmdTurn -v`
Expected: FAIL —— `undefined: cmdTurn`

- [ ] **Step 3: 实现**

新建 `cmd/engram/turn.go`：

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/turncapture"
)

// turnUsageExitCode is the only non-zero exit `engram turn` ever produces, and
// only for a malformed invocation. Every runtime outcome — safety valve
// engaged, project not enabled, transcript missing, network down — exits 0:
// this command runs from a Stop hook after every single turn, and a non-zero
// exit there is noise the user can do nothing about. A usage error, by
// contrast, only happens when a human typed the command, and silently
// succeeding on a typo'd flag wastes their afternoon.
const turnUsageExitCode = 2

// defaultTurnMaxBytes caps a single merged turn message. 32 KiB is generous for
// prose while keeping any one turn from dominating a conversation's extraction
// budget.
const defaultTurnMaxBytes = 32768

func printTurnUsage() {
	fmt.Fprintln(os.Stderr, "usage: engram turn --transcript <path> [--session <id>] [--cwd <dir>] [--verbose]")
}

// cmdTurn appends the transcript's last completed turn to the project's
// MemoryLake conversation, when that project has per-turn conversation sync
// enabled. Invoked by the Claude Code Stop hook once per turn; see
// docs/superpowers/specs/2026-08-06-memorylake-turn-sync-design.md.
func cmdTurn() {
	sessionID, transcript, cwd, verbose, ok := parseTurnArgs()
	if !ok {
		return
	}

	// Global safety valve: honored before anything else so it is impossible for
	// this path to reach MemoryLake while the valve is closed.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ENGRAM_BACKEND")), "sqlite") {
		return
	}

	project := detectProject(cwd)

	enab, err := loadMemorylakeEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		logTurnFailure(project, sessionID, fmt.Errorf("load enablement: %w", err))
		return
	}
	entry, enabled := enab.IsEnabled(project)
	if !enabled || !entry.SyncConversations {
		// The overwhelmingly common case. Nothing has been opened, nothing has
		// been dialed, nothing has been logged.
		return
	}
	if entry.ProjID == "" {
		// Enabled by an older engram that never persisted the resolved project
		// id. Resolving it here would mean creating a MemoryLake project from a
		// fire-and-forget hook; let the next mem_save do it instead.
		return
	}

	turn, err := turncapture.LastTurn(transcript)
	if err != nil {
		logTurnFailure(project, sessionID, err)
		return
	}
	if sessionID == "" {
		sessionID = turn.SessionID
	}
	if sessionID == "" {
		return
	}

	content, mergeOK := turn.Merged(turnMaxBytes())
	if !mergeOK {
		// An interrupted or tool-only turn. Routine, so not logged.
		return
	}

	mlCfg := loadMemorylakeConfig()
	backend, err := memorylake.NewBackend(mlCfg, mlCfg.Workspace, entry.ProjID)
	if err != nil {
		logTurnFailure(project, sessionID, fmt.Errorf("construct backend: %w", err))
		return
	}

	msgID, err := backend.AppendTurn(sessionID, content)
	if err != nil {
		logTurnFailure(project, sessionID, fmt.Errorf("append turn: %w", err))
		return
	}

	if verbose {
		fmt.Printf("appended turn to conversation %s (message %s, %d bytes)\n", sessionID, msgID, len(content))
	}
}

// parseTurnArgs reads the flags by hand, matching every other subcommand in
// this binary (there is no flag framework here). ok is false when the caller
// should return immediately because exitFunc has already been invoked.
func parseTurnArgs() (sessionID, transcript, cwd string, verbose, ok bool) {
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--session":
			if i+1 < len(os.Args) {
				sessionID = os.Args[i+1]
				i++
			}
		case "--transcript":
			if i+1 < len(os.Args) {
				transcript = os.Args[i+1]
				i++
			}
		case "--cwd":
			if i+1 < len(os.Args) {
				cwd = os.Args[i+1]
				i++
			}
		case "--verbose":
			verbose = true
		default:
			fmt.Fprintf(os.Stderr, "engram: unknown flag %q\n", os.Args[i])
			printTurnUsage()
			exitFunc(turnUsageExitCode)
			return "", "", "", false, false
		}
	}

	if transcript == "" {
		fmt.Fprintln(os.Stderr, "engram: --transcript <path> is required")
		printTurnUsage()
		exitFunc(turnUsageExitCode)
		return "", "", "", false, false
	}
	return sessionID, transcript, cwd, verbose, true
}

// turnMaxBytes reads the merged-message ceiling from the environment, tolerating
// garbage the same way internal/memorylake's config does.
func turnMaxBytes() int {
	if v := strings.TrimSpace(os.Getenv("ENGRAM_TURN_MAX_BYTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultTurnMaxBytes
}

// logTurnFailure appends one line to ~/.engram/logs/turn.log.
//
// Per-turn sync is fire-and-forget: a failure is never retried and never
// reaches the terminal (a Stop hook's stderr can flash in some terminals), so
// this file is its only trace. Everything here is best-effort — a logging
// failure is swallowed, because failing to log must not become a second
// failure mode.
func logTurnFailure(project, sessionID string, cause error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".engram", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "turn.log")

	// Diagnostic log, not an audit log: past 1 MiB start over rather than
	// growing without bound or managing rotated files.
	if st, statErr := os.Stat(path); statErr == nil && st.Size() > 1<<20 {
		_ = os.Remove(path)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "%s project=%s session=%s error=%v\n",
		time.Now().UTC().Format(time.RFC3339), project, sessionID, cause)
}
```

`cmd/engram/main.go` 三处接线：

**(a) `shouldCheckForUpdates`** —— 把 `turn` 加进豁免列表。这条最容易漏：不加的话每一轮对话都会去 GitHub 查一次版本。

```go
	switch command {
	case "mcp", "serve", "protocol-mode", "turn":
		return false
```

**(b) `handleConfigFreeCommand`** —— `turn` 不需要 `store.Config`，也绝不该打开 SQLite。

```go
	case "turn":
		cmdTurn()
		return true
```

**(c) `main` 的 switch** —— 加一个 case，让规范的命令列表保持完整（`version` / `help` 同样在两处都有，本条与之一致；实际执行走的是 (b) 的快路径）。

```go
	case "turn":
		cmdTurn()
```

**(d) `printUsage`** —— 在 `memorylake conversations` 那段之后插入：

```
  turn --transcript <path> [--session <id>] [--cwd <dir>] [--verbose]
                     (internal, invoked by agent hooks) Append the transcript's last
                     completed turn to the project's MemoryLake conversation. No-op
                     unless the project has `memorylake conversations` enabled.
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/engram/ -run CmdTurn -v`
Expected: PASS（7 个测试）

Run: `go test ./...`
Expected: PASS

手工验证一次真实幂等性（可选但推荐 —— 需要项目已 enable 且配好 api-key）：

```bash
go build ./cmd/engram
./engram turn --verbose \
  --session "$(ls -t ~/.claude/projects/*/*.jsonl | head -1 | xargs basename | sed 's/\.jsonl$//')" \
  --transcript "$(ls -t ~/.claude/projects/*/*.jsonl | head -1)" \
  --cwd "$PWD"
```

- [ ] **Step 5: 提交**

```bash
git add cmd/engram/turn.go cmd/engram/turn_test.go cmd/engram/main.go
git commit -m "feat(memorylake): add 'engram turn' to sync one completed turn

Runs from the Claude Code Stop hook after every turn, so the not-enabled path
is the one that matters: it reads one small JSON file and returns without
opening SQLite, dialing MemoryLake, or checking for updates ('turn' is added
to shouldCheckForUpdates' exemption list for exactly that reason).

Runtime problems always exit 0 and leave a line in ~/.engram/logs/turn.log;
only a malformed invocation exits non-zero, so a typo'd flag is visible to a
human without ever surfacing anything to a hook-driven run."
```

---

### Task 8: Stop hook 接线

让它真正在每轮之后自动跑起来。

**Files:**
- Create: `plugin/claude-code/scripts/turn-capture.sh`
- Modify: `plugin/claude-code/hooks/hooks.json`
- Test: `plugin/turn_capture_hook_test.go`（新建）

**Interfaces:**
- Consumes: `engram turn` CLI（Task 7）
- Produces: 无 Go 接口；产出的是 `Stop` 数组里第二个 hook 条目

- [ ] **Step 1: 写失败的测试**

新建 `plugin/turn_capture_hook_test.go`：

```go
package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTurnCaptureHookIsRegistered checks the Stop hook wiring: the existing
// session-stop entry must survive, the new turn-capture entry must be present,
// and it must be async so it never delays the user's reply.
func TestTurnCaptureHookIsRegistered(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "plugin", "claude-code", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Async   bool   `json:"async"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("hooks.json must stay valid JSON: %v", err)
	}

	stop := cfg.Hooks["Stop"]
	var sawSessionStop, sawTurnCapture bool
	for _, group := range stop {
		for _, h := range group.Hooks {
			if strings.Contains(h.Command, "session-stop.sh") {
				sawSessionStop = true
			}
			if strings.Contains(h.Command, "turn-capture.sh") {
				sawTurnCapture = true
				if !h.Async {
					t.Error("turn-capture must be async so it never delays the reply")
				}
				if h.Command != "\"${CLAUDE_PLUGIN_ROOT}/scripts/turn-capture.sh\"" {
					t.Errorf("command must be the quoted plugin-root form, got %q", h.Command)
				}
			}
		}
	}
	if !sawSessionStop {
		t.Error("the existing session-stop hook must not be removed")
	}
	if !sawTurnCapture {
		t.Error("turn-capture.sh must be registered on Stop")
	}
}

// TestTurnCaptureScriptDegradesGracefully pins the three properties that keep
// this hook from ever breaking a session: it checks the binary exists, it exits
// 0 unconditionally, and it swallows the CLI's exit code.
func TestTurnCaptureScriptDegradesGracefully(t *testing.T) {
	path := filepath.Join(repoRoot(), "plugin", "claude-code", "scripts", "turn-capture.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)

	for _, want := range []string{
		"command -v engram",
		"|| true",
		"exit 0",
		"engram turn",
		"--transcript",
		"--session",
		"--cwd",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("turn-capture.sh must contain %q", want)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("turn-capture.sh must be executable")
	}
}
```

> `repoRoot()` 已经存在于 `plugin/assets_test.go`（`plugin/assets_test.go -> up one directory`）。如果它是私有且名字不同，用同一文件里实际的那个 helper 名字。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./plugin/ -run TurnCapture -v`
Expected: FAIL —— `turn-capture.sh` 不存在、hooks.json 里没有该条目

- [ ] **Step 3: 实现**

新建 `plugin/claude-code/scripts/turn-capture.sh`：

```bash
#!/bin/bash
# Engram — per-turn conversation sync for Claude Code (async)
#
# Feeds each completed turn (one user message plus the assistant's final reply)
# into the project's MemoryLake conversation, so MemoryLake's own extraction
# pipeline can mint memories from it without the agent having to remember to
# call mem_save.
#
# This script only moves values; it decides nothing. Whether the project has
# per-turn sync enabled is `engram turn`'s call — the project name must be
# resolved by Go (it reads .engram/config, which detect_project in _helpers.sh
# does not), or a project renamed via config would silently never sync.
#
# Never blocks and never fails a session: missing binary, old binary, broken
# network — all exit 0.

INPUT=$(cat)
command -v engram >/dev/null 2>&1 || exit 0

SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')

[ -n "$SESSION_ID" ] && [ -n "$TRANSCRIPT" ] || exit 0

# No backgrounding needed — hooks.json marks this hook async:true, so Claude
# Code does not wait for it. `|| true` swallows the non-zero an older engram
# returns for an unknown subcommand.
engram turn --session "$SESSION_ID" --transcript "$TRANSCRIPT" --cwd "$CWD" \
  >/dev/null 2>&1 || true

exit 0
```

```bash
chmod +x plugin/claude-code/scripts/turn-capture.sh
```

`plugin/claude-code/hooks/hooks.json` 的 `Stop` 数组追加第二个条目（**保留现有 `session-stop.sh` 条目**）：

```json
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"${CLAUDE_PLUGIN_ROOT}/scripts/session-stop.sh\"",
            "timeout": 5,
            "async": true
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"${CLAUDE_PLUGIN_ROOT}/scripts/turn-capture.sh\"",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ]
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./plugin/ -v`
Expected: PASS（含既有的 `TestHooksJSONPluginRootIsQuoted` 和 `TestPluginAssetsDoNotLeakSpanishTriggers`——新脚本会被 `scripts/*.sh` glob 自动纳入）

Run: `go test ./...`
Expected: PASS

手工验证 hook 输入解析：

```bash
echo '{"session_id":"sess-x","transcript_path":"/tmp/nope.jsonl","cwd":"'"$PWD"'"}' \
  | bash plugin/claude-code/scripts/turn-capture.sh; echo "exit=$?"
```
Expected: `exit=0`

- [ ] **Step 5: 提交**

```bash
git add plugin/claude-code/scripts/turn-capture.sh plugin/claude-code/hooks/hooks.json plugin/turn_capture_hook_test.go
git commit -m "feat(memorylake): register the per-turn conversation sync Stop hook

A second async Stop entry alongside the existing session-stop one. The script
only moves values from the hook payload into 'engram turn' — the project name
is resolved in Go on purpose, because _helpers.sh's detect_project does not
read .engram/config and a project renamed there would never sync."
```

---

### Task 9: 端到端流程测试与文档

最后一道：一个跨全部零件的有状态 mock 流程测试，加上文档。合成一个任务是因为文档描述的正是这个流程验证出来的行为——分开会让文档先于验证落地。

**Files:**
- Create: `internal/memorylake/turnsync_e2e_test.go`
- Modify: `DOCS.md`（"MemoryLake Backend" 一节）
- Modify: `README.md`
- Modify: `docs/INSTALL.zh-CN.md`（中文安装文档）

**Interfaces:**
- Consumes: 全部前置任务
- Produces: 无

- [ ] **Step 1: 写失败的测试**

新建 `internal/memorylake/turnsync_e2e_test.go`：

```go
package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// convStore is a stateful mock of the conversation endpoints: it enforces the
// same custom_id semantics the real MemoryLake has — conversation creation
// rejects a duplicate custom_id (recoverable by GET), and a message whose
// custom_id was already posted resolves to the existing message instead of
// creating a second one.
type convStore struct {
	mu       sync.Mutex
	convByID map[string]string   // conversation custom_id -> conversation id
	msgs     map[string][]string // conversation id -> message texts, in order
	msgIDs   map[string]string   // message custom_id -> message id
	seq      int
}

func newConvStore() *convStore {
	return &convStore{
		convByID: map[string]string{},
		msgs:     map[string][]string{},
		msgIDs:   map[string]string{},
	}
}

func (c *convStore) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()

		ok := func(data any) {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
		}

		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if _, exists := c.convByID[body.CustomID]; exists {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"error":   map[string]any{"code": "CUSTOM_ID_CONFLICT", "message": "exists"},
				})
				return
			}
			c.seq++
			id := "conv-" + itoa(c.seq)
			c.convByID[body.CustomID] = id
			ok(map[string]any{"id": id})

		case r.Method == "GET" && hasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/memories/conversations/"):
			custom := lastPathSegment(r.URL.Path)
			id, exists := c.convByID[custom]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"error":   map[string]any{"code": "NOT_FOUND", "message": "no such conversation"},
				})
				return
			}
			ok(map[string]any{"id": id})

		case r.Method == "POST" && hasSuffix(r.URL.Path, "/messages"):
			convID := pathSegment(r.URL.Path, "conversations")
			var body struct {
				CustomID string `json:"custom_id"`
				Content  []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if id, exists := c.msgIDs[body.CustomID]; exists {
				// MemoryLake's message idempotency: same custom_id, same message.
				ok(map[string]any{"id": id})
				return
			}
			c.seq++
			id := "msg-" + itoa(c.seq)
			c.msgIDs[body.CustomID] = id
			text := ""
			if len(body.Content) > 0 {
				text = body.Content[0].Text
			}
			c.msgs[convID] = append(c.msgs[convID], text)
			ok(map[string]any{"id": id})

		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
}

// TestTurnSyncFlow_PromptSuppressedAndTurnAppendedOnce is the whole feature in
// one test: with suppression on, the prompt append makes no request, the turn
// lands as exactly one message on the session's conversation, and replaying
// the same turn does not add a second one.
func TestTurnSyncFlow_PromptSuppressedAndTurnAppendedOnce(t *testing.T) {
	cs := newConvStore()
	srv := httptest.NewServer(cs.handler(t))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	b.SetSkipPromptAppend(true)

	// 1. The user's prompt is persisted through the normal path — and must not
	//    reach MemoryLake, because the merged turn carries it.
	if _, err := b.AddPrompt(store.AddPromptParams{
		SessionID: "sess-e2e", Project: "acme", Content: "fix the uploader",
	}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	// 2. The turn arrives.
	content := "**User:**\nfix the uploader\n\n**Assistant:**\ndone"
	if _, err := b.AppendTurn("sess-e2e", content); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	// 3. A replay of the same turn (a re-run, a manual backfill) is a no-op.
	if _, err := b.AppendTurn("sess-e2e", content); err != nil {
		t.Fatalf("AppendTurn (replay): %v", err)
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	convID, exists := cs.convByID["sess-e2e"]
	if !exists {
		t.Fatal("the turn must create a conversation keyed by the session id")
	}
	msgs := cs.msgs[convID]
	if len(msgs) != 1 {
		t.Fatalf("conversation holds %d messages, want exactly 1: %#v", len(msgs), msgs)
	}
	if msgs[0] != content {
		t.Fatalf("stored message = %q, want the merged turn verbatim", msgs[0])
	}
}
```

> 这个测试用到四个小工具函数 `itoa` / `hasPrefix` / `hasSuffix` / `pathSegment` / `lastPathSegment`。**不要新写**：改用标准库 —— `strconv.Itoa`、`strings.HasPrefix`、`strings.HasSuffix`，路径段用 `strings.Split(r.URL.Path, "/")` 就地取。写实现时把这些调用直接替换成标准库形式并补上 import。查询串（`?by_custom_id=true`）在 `r.URL.Path` 里不出现，所以 `lastPathSegment` 直接取最后一段即可。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/memorylake/ -run TurnSyncFlow -v`
Expected: FAIL —— 编译错误（未定义的 helper），换成标准库后应转为 PASS

- [ ] **Step 3: 实现**

本步无生产代码——前八个任务已经实现了全部行为。把测试里的 helper 换成标准库调用让它跑通，然后写文档。

**`DOCS.md`** 在 "MemoryLake Backend" 一节末尾追加：

```markdown
### Per-turn conversation sync

By default, memories only reach MemoryLake when the agent decides to call
`mem_save`. Per-turn conversation sync removes that dependency: with it on,
every completed turn — one user message plus the assistant's final reply — is
appended to the project's MemoryLake conversation the moment the turn ends, and
MemoryLake's own extraction pipeline mints memories from it in the background.

```bash
engram memorylake conversations enable  --project <name>
engram memorylake conversations disable --project <name>
engram memorylake status   # shows conversations: on|off per enabled project
```

Requires the project to be MemoryLake-enabled first (`engram memorylake enable
--project <name>`) — the sync writes into that project's MemoryLake
conversation and has nowhere to go without one.

**What a "turn" is.** The user's message (including anything typed while the
assistant was still working) plus the assistant's final text reply. Thinking,
tool calls, and tool output are excluded. Both halves go into a single message
labelled `**User:**` / `**Assistant:**`; the conversation is keyed by the
agent's session id, so `--resume` continues the same conversation and `/clear`
starts a new one.

**Environment variables**

| Variable | Default | Effect |
|---|---|---|
| `ENGRAM_BACKEND=sqlite` | — | Global MemoryLake safety valve; also disables per-turn sync |
| `ENGRAM_TURN_MAX_BYTES` | `32768` | Byte ceiling for one merged turn message; over-long turns keep head and tail with a truncation marker between |
| `ENGRAM_TURN_MAX_TRANSCRIPT_BYTES` | `67108864` | Transcripts above this are read from the tail instead of whole |
| `ENGRAM_TURN_TAIL_WINDOW_BYTES` | `2097152` | Size of that tail window |

**Limitations**

- **Claude Code only.** Driven by Claude Code's `Stop` hook and its transcript
  format. Codex, OpenCode and Pi are not covered.
- **Interrupted turns are not captured.** Claude Code does not fire `Stop` when
  the user presses ESC.
- **Failures are dropped, not retried.** A turn lost to a network outage is
  gone; the failure is recorded in `~/.engram/logs/turn.log` and nowhere else.
- **Every message is stored with role `USER`.** MemoryLake derives a message's
  role from its actor's type and only HUMAN actors can be created through the
  API, so the speaker is carried in the message text instead.
- **No tool trace.** Which files changed and which commands ran do not leave
  the machine through this path.
- **Subagent turns are skipped.**
- **While it is on, prompts are not appended separately** — the merged turn
  already contains the user's text, so appending it twice would skew
  extraction. Prompt storage is write-only on this backend, so nothing local
  is lost.
```

**`README.md`** 在 MemoryLake 段落里加一句：

```markdown
Enabled projects can additionally opt into per-turn conversation sync
(`engram memorylake conversations enable --project <name>`), which appends each
completed turn to MemoryLake and lets its extraction pipeline mint memories
without the agent having to call `mem_save`. See "Per-turn conversation sync"
in `DOCS.md`.
```

**`docs/INSTALL.zh-CN.md`** 同样加一句：

```markdown
已启用 MemoryLake 的项目还可以开启逐轮对话同步：

```bash
engram memorylake conversations enable --project <项目名>
```

开启后每完成一轮问答都会自动写入 MemoryLake，由云端抽取成记忆，不再依赖模型主动保存。仅支持 Claude Code；被 ESC 打断的轮次不会入库。详见 `DOCS.md` 的 "Per-turn conversation sync"。
```

`plugin/claude-code/skills/memory/SKILL.md` **不改** —— 这个功能对模型完全透明，不该占它的上下文。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/memorylake/ -run TurnSyncFlow -v`
Expected: PASS

Run: `go test ./...`
Expected: PASS

Run: `go test -tags e2e ./internal/server/...`
Expected: PASS（本计划未触及 server，应保持原状）

Run: `go build ./cmd/engram`
Expected: 构建成功

- [ ] **Step 5: 提交**

```bash
git add internal/memorylake/turnsync_e2e_test.go DOCS.md README.md docs/INSTALL.zh-CN.md
git commit -m "test(memorylake): end-to-end turn sync flow, plus docs

One stateful-mock test over the whole feature: prompt append suppressed,
turn appended as exactly one message on the session's conversation, replay
of the same turn a no-op. Documents the switch, what counts as a turn, the
environment variables, and all seven limitations."
```

---

## Self-Review

**1. Spec coverage**

| Spec 节 | 覆盖任务 |
|---|---|
| §2 D1（开关依附） | Task 1（`SetConversationSync` 报错）+ Task 2（CLI 错误路径） |
| §2 D2（只写问答） | Task 3（丢弃 thinking/tool_use/tool_result） |
| §2 D3（一条合并消息） | Task 4（`Merged` 渲染） |
| §2 D4（抑制 prompt 追加） | Task 6 |
| §2 D5（Stop hook → CLI） | Task 7 + Task 8 |
| §2 D6（发完就忘 + 截断） | Task 4（截断）+ Task 7（日志、不重试） |
| §3 组件与边界 | Task 3/4（新包）、Task 5（turns.go）、Task 7（turn.go）、Task 8（脚本） |
| §4 配置模型与 CLI 表面 | Task 1 + Task 2 |
| §4 全局安全阀 | Task 7（`ENGRAM_BACKEND=sqlite`，含测试） |
| §5 数据流 8 步 | Task 7（`cmdTurn` 逐步实现） |
| §6 轮次切分算法 | Task 3（含全部表格行的测试） |
| §6 大文件处理 | Task 3（`readLines` + 尾窗测试） |
| §7 合并格式与截断 | Task 4 |
| §8 `AppendTurn` | Task 5 |
| §9 prompt 抑制 | Task 6 |
| §10 CLI 契约与退出码 | Task 7（退出码 2 / 0 各有测试） |
| §11 Hook 集成 | Task 8 |
| §12 错误处理与日志 | Task 7（`logTurnFailure` + 日志断言） |
| §13 环境变量 | Task 3（两个 transcript 变量）+ Task 7（`ENGRAM_TURN_MAX_BYTES`、安全阀）+ Task 9（文档表格） |
| §14 已知取舍 | Task 9（文档 Limitations 七条） |
| §15 测试策略 | Task 1–9 各自的测试步 |
| §16 文档 | Task 9 |
| §17 落地顺序 | 任务顺序即此顺序 |
| §18 仓库规则 | Global Constraints + 各任务 commit message |

无缺口。

**2. Placeholder scan**

写完计划后回查并当场定死了四处原本留作"实现时核对"的东西：
- Task 5 / Task 9 的冲突响应体：`error_code` 是顶层字段（`identity_test.go:425`），已写进代码
- Task 7 的 `turnProjectDir`：`.engram/config.json` + 字段 `project_name`（`detect.go:174`、`:220`），已写进代码
- Task 9 的中文安装文档：`docs/INSTALL.zh-CN.md`
- Task 5 注释里的中英混排笔误

仅剩一处刻意的"实现时替换"：Task 9 测试里的五个 helper 要换成标准库调用（`strconv.Itoa` / `strings.HasPrefix` / `strings.HasSuffix` / `strings.Split`）。这是有意为之——写成占位 helper 让 mock 的意图更易读，替换规则在紧随的注记里逐一给出。另有一处 `.engram/config.json` 在非 git 目录下不生效时的退路，同样给了具体改法。

**3. Type consistency**

- `SyncConversations` 字段名：Task 1 定义，Task 2/6/7 使用，一致。
- `IsConversationSyncEnabled` / `SetConversationSync`：Task 1 定义，Task 2 与测试使用，一致。
- `Turn{SessionID, UserText, AssistantText}`：Task 3 定义，Task 4 的 `Merged` 与 Task 7 的 `cmdTurn` 使用，一致。
- `Merged(maxBytes int) (string, bool)`：Task 4 定义，Task 7 按 `content, mergeOK :=` 使用，一致。
- `AppendTurn(sessionID, content string) (string, error)`：Task 5 定义，Task 7 与 Task 9 使用，一致。
- `SetSkipPromptAppend(v bool)`：Task 6 定义，`routing.go` 与 Task 9 测试使用，一致。
- `cmdTurn()` / `cmdMemorylakeConversations()` 均无参数，与 `main.go` 的调用点一致。
