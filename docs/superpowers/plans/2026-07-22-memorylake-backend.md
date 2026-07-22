# Engram × MemoryLake 逐 project 可选后端 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Engram 的 `mem_*` 工具在**用户显式 enable 的 project 上**把读写落到 MemoryLake V3 facts(挂 `engram` workspace),未 enable 的 project 保持现有 SQLite 后端不变。

**Architecture:** 抽出一个 `MemoryBackend` Go 接口(方法面 = 现有 handler 对 `*store.Store` 的调用面,货币类型沿用 `store.*`),`*store.Store` 直接实现它(SQLite 默认后端);新增 `internal/memorylake` 包实现同一接口(V3 REST client + 身份解析 + Observation⇄fact 映射 + 异步抽取回填 + 语义/子串检索)。handler 依「逐 project 启用清单」注入其一。`MemoryLakeBackend` 内部维护 `int64 ↔ fact-id` 映射,使接口保持 int64、对外工具契约零改动。

**Tech Stack:** Go 1.x(CGO_ENABLED=0)、标准库 `net/http`/`encoding/json`、现有 `modernc.org/sqlite`(默认后端保留)、`mark3labs/mcp-go`(工具注册,不变)。

## Global Constraints

- 构建:`CGO_ENABLED=0`,纯 Go,`go build ./cmd/engram` 必须通过;测试 `go test ./...`。
- **只改 engram 服务**;不改 `plugin/`、agent 指令、MCP 工具名与入参 schema。
- Conventional commits:`feat(memorylake):` / `refactor(mcp):` / `docs(...)`;**不加 `Co-Authored-By` trailer**(仓库规则)。
- MemoryLake 鉴权仅 `Authorization: Bearer <key>`;workspace/project 只走 URL path,不发 tenant/workspace header。
- 外部 base 前缀固定 `/openapi/memorylake`;响应统一 `{success,message,data,error_code}` 包装,client 必须解包。
- 默认后端 = SQLite;MemoryLake 仅对启用清单内 project 生效。全局安全阀 `ENGRAM_BACKEND=sqlite` 可强制全走 SQLite。
- 参考规格:`docs/superpowers/specs/2026-07-22-memorylake-backend-design.md`(本计划的权威来源)。
- **禁止修改 `./memorylake/` 目录下任何代码**(那是 4 个 MemoryLake 上游子仓库,仅作只读 API 参考)。⚠️ 命名易混:本计划**新增的 Engram 包路径是 `internal/memorylake/`**,与仓库根下的参考目录 `./memorylake/` 是**两个完全不同的东西**;所有改动只落在 `internal/`、`cmd/engram/`、根级 `*.md`。

### 计划级精化(相对 spec §4.3 的偏差,已在架构中采用)
- spec §4.3 说"对外 id 从 int 改为 fact-id 字符串"。**本计划改为**:`MemoryBackend` 接口保持 `int64` id,`MemoryLakeBackend` 内部维护 `int64↔fact-id` 双向映射(持久化于 `~/.engram/memorylake-idmap-<project>.json`)。好处:mcp handler 与工具入参零改动,重构面最小。语义等价,建议采纳。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/mcp/backend.go`(新) | `MemoryBackend` 接口定义 + 编译期断言 `*store.Store` 实现它 |
| `internal/mcp/mcp.go`(改) | handler 注入类型 `*store.Store` → `MemoryBackend`;新增逐 project 选择器 |
| `internal/memorylake/config.go`(新) | 环境变量配置 + 启用清单读写(`~/.engram/memorylake.json`) |
| `internal/memorylake/client.go`(新) | V3 REST client:do/auth/解包/错误映射/分页 |
| `internal/memorylake/identity.go`(新) | workspace/project/actor 解析与幂等 provision + 缓存 |
| `internal/memorylake/idmap.go`(新) | `int64↔fact-id` 本地映射 |
| `internal/memorylake/mapper.go`(新) | Observation ⇄ fact+metadata 编解码 |
| `internal/memorylake/writequeue.go`(新) | append→轮询抽取→PATCH 回填 |
| `internal/memorylake/search.go`(新) | 语义 + `fact_fuzzy` 合并 + 客户端过滤 |
| `internal/memorylake/backend.go`(新) | `MemoryLakeBackend` 实现 `MemoryBackend` |
| `cmd/engram/main.go`(改) | 新增 `memorylake enable/disable/status` 子命令 |
| `DOCS.md` / `CLAUDE.md`(改) | 配置项与后端选择说明 |

---

## Task 1: 定义 `MemoryBackend` 接口

**Files:**
- Create: `internal/mcp/backend.go`
- Test: `internal/mcp/backend_test.go`

**Interfaces:**
- Produces: `MemoryBackend` 接口(方法签名逐字取自现有 `*store.Store`);编译期断言 `var _ MemoryBackend = (*store.Store)(nil)`。

- [ ] **Step 1: 写编译期断言测试(先失败)**

```go
// internal/mcp/backend_test.go
package mcp

import (
	"testing"

	"github.com/[org]/engram/internal/store" // 用仓库实际 module path
)

func TestStoreSatisfiesMemoryBackend(t *testing.T) {
	var _ MemoryBackend = (*store.Store)(nil) // 不实现则编译失败
}
```

> 实施者注:module path 用 `head -1 go.mod` 查得的真实值替换 `[org]/engram`。

- [ ] **Step 2: 运行,确认因 `MemoryBackend` 未定义而失败**

Run: `go test ./internal/mcp/ -run TestStoreSatisfiesMemoryBackend`
Expected: FAIL —— `undefined: MemoryBackend`

- [ ] **Step 3: 定义接口(方法面 = Task 前置调研枚举的 28 个方法)**

```go
// internal/mcp/backend.go
package mcp

import "github.com/[org]/engram/internal/store"

// MemoryBackend 抽象 mem_* handler 依赖的存储能力。
// *store.Store(SQLite)与 *memorylake.MemoryLakeBackend 各实现一份。
// 货币类型沿用 store 包,保证 handler 主体零改动。
type MemoryBackend interface {
	// 观测 CRUD
	AddObservation(p store.AddObservationParams) (int64, error)
	GetObservation(id int64) (*store.Observation, error)
	UpdateObservation(id int64, p store.UpdateObservationParams) (*store.Observation, error)
	DeleteObservation(id int64, hardDelete bool) error
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
	Timeline(observationID int64, before, after int) (*store.TimelineResult, error)
	FormatContext(project, scope string) (string, error)
	Stats() (*store.Stats, error)
	MaxObservationLength() int
	// pin / review
	PinObservation(id int64) error
	UnpinObservation(id int64) error
	ObservationsNeedingReview(project string, limit int) ([]store.Observation, error)
	MarkReviewed(id int64) error
	// sessions
	CreateSession(p store.CreateSessionParams) error
	GetSession(id string) (*store.Session, error)
	EndSession(id string, summary string) error
	MostRecentActiveSession(project string) (*store.Session, error)
	RecentSessions(project string, limit int) ([]store.SessionSummary, error)
	// prompts / passive
	AddPrompt(p store.AddPromptParams) (int64, error)
	AddPromptIfMissing(p store.AddPromptParams) (int64, bool, error)
	PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error)
	// projects
	ProjectExists(name string) (bool, error)
	ListProjectNames() ([]string, error)
	CountObservationsForProject(project string) (int, error)
	MergeProjects(sources []string, canonical string) (*store.MergeResult, error)
	// relations / 冲突
	FindCandidates(savedID int64, opts store.CandidateOptions) ([]store.Candidate, error)
	GetRelationsForObservations(ids []int64) (map[string]store.ObservationRelations, error)
	JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error)
	JudgeBySemantic(p store.JudgeBySemanticParams) (string, error)
}

var _ MemoryBackend = (*store.Store)(nil)
```

> 实施者注:逐个方法**必须与 `internal/store` 中的真实签名逐字一致**。用
> `grep -nE "^func \(s \*Store\) (AddObservation|GetObservation|...)\(" internal/store/*.go`
> 核对每个方法的参数/返回类型与包内类型名(如 `CreateSessionParams` 若实为 `s.CreateSession(id, dir string)` 则据实改)。不一致会编译失败,以编译器为准修正接口。

- [ ] **Step 4: 运行,确认通过(若报某方法签名不符,按编译器提示逐字对齐)**

Run: `go test ./internal/mcp/ -run TestStoreSatisfiesMemoryBackend`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/backend.go internal/mcp/backend_test.go
git commit -m "refactor(mcp): extract MemoryBackend interface satisfied by store.Store"
```

---

## Task 2: handler 注入改为 `MemoryBackend` + 逐 project 选择器(默认恒 SQLite)

**Files:**
- Modify: `internal/mcp/mcp.go`(`NewServer*`、`registerTools`、各 `handleXxx(s *store.Store, ...)` 签名)
- Create: `internal/mcp/selector.go`
- Test: `internal/mcp/selector_test.go`

**Interfaces:**
- Consumes: `MemoryBackend`(Task 1)
- Produces: `type BackendSelector func(project string) MemoryBackend`;`NewServerWithConfig` 接收一个 selector;默认 selector 恒返回 SQLite。

- [ ] **Step 1: 写 selector 测试(先失败)**

```go
// internal/mcp/selector_test.go
package mcp
import "testing"
func TestDefaultSelectorAlwaysSQLite(t *testing.T) {
	sqlite := &fakeBackend{name: "sqlite"}
	sel := StaticSelector(sqlite)
	if sel("anything") != sqlite {
		t.Fatal("default selector must always return the sqlite backend")
	}
}
// fakeBackend: 最小实现 MemoryBackend 的桩(仅本测试用,方法可 panic/返回零值)
```

> 实施者注:`fakeBackend` 需实现全部接口方法;可用 `//go:generate` 或手写空实现,body 里 `panic("unused")` 即可,只有本测试引用的字段有意义。

- [ ] **Step 2: 运行,确认失败(`StaticSelector` 未定义)**

Run: `go test ./internal/mcp/ -run TestDefaultSelectorAlwaysSQLite`
Expected: FAIL —— `undefined: StaticSelector`

- [ ] **Step 3: 实现 selector**

```go
// internal/mcp/selector.go
package mcp

// BackendSelector 依 project 决定用哪个后端。project 为空表示未知/默认。
type BackendSelector func(project string) MemoryBackend

// StaticSelector 恒返回同一后端(默认行为:全走 SQLite)。
func StaticSelector(b MemoryBackend) BackendSelector {
	return func(string) MemoryBackend { return b }
}
```

- [ ] **Step 4: 把 handler/注册函数的 `s *store.Store` 改为 selector 驱动**

机械改动(逐字模式):
1. `registerTools`、`NewServerWithConfig`、`newServerWithActivity` 等新增参数 `sel BackendSelector`;`NewServer(s *store.Store)` 保持兼容:内部 `sel := StaticSelector(s)`。
2. 各 `handleXxx(s *store.Store, ...)` 改为 `handleXxx(sel BackendSelector, ...)`;函数体开头解析出 project 后 `s := sel(project)`,其余 `s.Xxx(...)` 调用**不变**(因 `s` 现在是 `MemoryBackend`,方法同名同签)。
   - 对"project 尚不可知就要用 s"的 handler(如 `mem_current_project`/`mem_stats` 全局):`s := sel("")`。
3. 包级 `var suggestTopicKey = store.SuggestTopicKey`、`ensureImplicitSessionWithCWD(s *store.Store,...)` 等纯函数/辅助:凡只读且 SQLite 专有的,保持接收 `*store.Store` 并仅在 SQLite 分支调用;或提升到接口(择就近)。

- [ ] **Step 5: 运行全量测试,确认现有 mcp 测试仍通过(零行为变化)**

Run: `go build ./cmd/engram && go test ./internal/mcp/`
Expected: PASS(所有既有 `mcp_test.go` 用例不变)

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/mcp.go internal/mcp/selector.go internal/mcp/selector_test.go
git commit -m "refactor(mcp): route mem_* handlers through per-project BackendSelector"
```

---

## Task 3: MemoryLake 配置与启用清单 + CLI

**Files:**
- Create: `internal/memorylake/config.go`, `internal/memorylake/config_test.go`
- Modify: `cmd/engram/main.go`(新增 `memorylake` 子命令)

**Interfaces:**
- Produces:
  - `type Config struct { BaseURL, APIKey, Workspace, Actor string; TimeoutMS, ExtractPollMS, ExtractMaxWaitMS int }`
  - `func LoadConfig() Config`(读环境变量,填默认)
  - `type Enablement struct { EnabledProjects map[string]ProjectEntry }`;`ProjectEntry{ ProjID, EnabledAt string }`
  - `func LoadEnablement(path string) (*Enablement, error)` / `(*Enablement) Save(path) error`
  - `func (*Enablement) IsEnabled(project string) (ProjectEntry, bool)`
  - `func DefaultEnablementPath() string` → `~/.engram/memorylake.json`

- [ ] **Step 1: 写 enablement 读写测试(先失败)**

```go
// internal/memorylake/config_test.go
package memorylake
import ("path/filepath"; "testing")

func TestEnablementRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memorylake.json")
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{}}
	e.EnabledProjects["acme"] = ProjectEntry{ProjID: "proj-1", EnabledAt: "2026-07-22T00:00:00Z"}
	if err := e.Save(p); err != nil { t.Fatal(err) }
	got, err := LoadEnablement(p)
	if err != nil { t.Fatal(err) }
	if entry, ok := got.IsEnabled("acme"); !ok || entry.ProjID != "proj-1" {
		t.Fatalf("want proj-1 enabled, got %+v ok=%v", entry, ok)
	}
	if _, ok := got.IsEnabled("other"); ok { t.Fatal("other must not be enabled") }
}

func TestLoadEnablementMissingFileIsEmpty(t *testing.T) {
	got, err := LoadEnablement(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil { t.Fatalf("missing file must be empty, not error: %v", err) }
	if _, ok := got.IsEnabled("x"); ok { t.Fatal("empty enablement") }
}
```

- [ ] **Step 2: 运行,确认失败**

Run: `go test ./internal/memorylake/ -run TestEnablement`
Expected: FAIL —— `undefined: Enablement`（包尚不存在）

- [ ] **Step 3: 实现 config.go**

```go
// internal/memorylake/config.go
package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	BaseURL, APIKey, Workspace, Actor           string
	TimeoutMS, ExtractPollMS, ExtractMaxWaitMS  int
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil { return n }
	}
	return def
}

func LoadConfig() Config {
	ws := os.Getenv("ENGRAM_MEMORYLAKE_WORKSPACE")
	if ws == "" { ws = "engram" }
	return Config{
		BaseURL:          os.Getenv("ENGRAM_MEMORYLAKE_BASE_URL"),
		APIKey:           os.Getenv("ENGRAM_MEMORYLAKE_API_KEY"),
		Workspace:        ws,
		Actor:            os.Getenv("ENGRAM_MEMORYLAKE_ACTOR"),
		TimeoutMS:        envInt("ENGRAM_MEMORYLAKE_TIMEOUT_MS", 30000),
		ExtractPollMS:    envInt("ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS", 2000),
		ExtractMaxWaitMS: envInt("ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS", 30000),
	}
}

type ProjectEntry struct {
	ProjID    string `json:"proj_id"`
	EnabledAt string `json:"enabled_at"`
}
type Enablement struct {
	EnabledProjects map[string]ProjectEntry `json:"enabled_projects"`
}

func DefaultEnablementPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".engram", "memorylake.json")
}

func LoadEnablement(path string) (*Enablement, error) {
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) { return e, nil }
	if err != nil { return nil, err }
	if err := json.Unmarshal(b, e); err != nil { return nil, err }
	if e.EnabledProjects == nil { e.EnabledProjects = map[string]ProjectEntry{} }
	return e, nil
}

func (e *Enablement) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { return err }
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil { return err }
	return os.WriteFile(path, b, 0o644)
}

func (e *Enablement) IsEnabled(project string) (ProjectEntry, bool) {
	entry, ok := e.EnabledProjects[project]
	return entry, ok
}
```

- [ ] **Step 4: 运行,确认通过**

Run: `go test ./internal/memorylake/ -run TestEnablement`
Expected: PASS

- [ ] **Step 5: 加 CLI 子命令 `memorylake enable/disable/status`**

在 `cmd/engram/main.go` 的手写 subcommand switch 里加 `case "memorylake":`,分发 enable/disable/status:
- `enable --project <name> [--migrate]`:调用 identity(Task 5)的 `EnsureProject(name)` 得 `proj-id`,写入 enablement 并 `Save`;`--migrate` 触发 Task 9 的迁移(可后置为 TODO 提示"迁移在 Task 9 接入")。
- `disable --project <name>`:从清单删除并 `Save`。
- `status`:读清单,列每个已知 project 的后端(enabled→memorylake / 否→sqlite)。

> 参数解析照抄 `main.go` 内既有子命令的手写 flag 风格(无 cobra)。此步无独立单测,靠 `go build` + 手动 `engram memorylake status` 冒烟。

- [ ] **Step 6: 构建冒烟 + Commit**

```bash
go build ./cmd/engram && ./engram memorylake status
git add internal/memorylake/config.go internal/memorylake/config_test.go cmd/engram/main.go
git commit -m "feat(memorylake): config + per-project enablement list + CLI enable/disable/status"
```

---

## Task 4: V3 REST client(do/解包/错误/分页)

**Files:**
- Create: `internal/memorylake/client.go`, `internal/memorylake/client_test.go`

**Interfaces:**
- Consumes: `Config`(Task 3)
- Produces:
  - `type Client struct{...}`;`func NewClient(cfg Config) *Client`
  - `func (c *Client) doJSON(method, path string, body any, out any) error` —— 自动加 Bearer、拼 BaseURL、解 `{success,message,data,error_code}`、`success==false` 时返回 `*APIError`
  - `type APIError struct { Code, Message string; HTTP int }`(实现 `error`)

- [ ] **Step 1: 写 client 测试(httptest,先失败)**

```go
// internal/memorylake/client_test.go
package memorylake
import ("encoding/json"; "net/http"; "net/http/httptest"; "testing")

func TestDoJSONUnwrapsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" { t.Errorf("missing bearer") }
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "ws-1"}})
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	var out struct{ ID string `json:"id"` }
	if err := c.doJSON("GET", "/api/v3/workspaces/ws-1", nil, &out); err != nil { t.Fatal(err) }
	if out.ID != "ws-1" { t.Fatalf("got %q", out.ID) }
}

func TestDoJSONMapsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "Project not found", "error_code": "NOT_FOUND"})
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	err := c.doJSON("GET", "/api/v3/x", nil, nil)
	apiErr, ok := err.(*APIError)
	if !ok { t.Fatalf("want *APIError, got %T", err) }
	if apiErr.Code != "NOT_FOUND" { t.Fatalf("code=%q", apiErr.Code) }
}
```

- [ ] **Step 2: 运行,确认失败**

Run: `go test ./internal/memorylake/ -run TestDoJSON`
Expected: FAIL —— `undefined: NewClient`

- [ ] **Step 3: 实现 client.go**

```go
// internal/memorylake/client.go
package memorylake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type APIError struct {
	Code, Message string
	HTTP          int
}
func (e *APIError) Error() string { return fmt.Sprintf("memorylake %s (%d): %s", e.Code, e.HTTP, e.Message) }

type Client struct {
	base   string
	key    string
	http   *http.Client
}
func NewClient(cfg Config) *Client {
	return &Client{
		base: strings.TrimRight(cfg.BaseURL, "/"),
		key:  cfg.APIKey,
		http: &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond},
	}
}

type wrapper struct {
	Success   bool            `json:"success"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"error_code"`
	Data      json.RawMessage `json:"data"`
}

func (c *Client) doJSON(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil { return err }
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil { return err }
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil { req.Header.Set("Content-Type", "application/json") }
	resp, err := c.http.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var w wrapper
	if err := json.Unmarshal(raw, &w); err != nil {
		return &APIError{Code: "BAD_RESPONSE", Message: string(raw), HTTP: resp.StatusCode}
	}
	if !w.Success {
		return &APIError{Code: w.ErrorCode, Message: w.Message, HTTP: resp.StatusCode}
	}
	if out != nil && len(w.Data) > 0 {
		return json.Unmarshal(w.Data, out)
	}
	return nil
}
```

- [ ] **Step 4: 运行,确认通过**

Run: `go test ./internal/memorylake/ -run TestDoJSON`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/memorylake/client.go internal/memorylake/client_test.go
git commit -m "feat(memorylake): V3 REST client with ResponseWrapper unwrap and error mapping"
```

---

## Task 5: 身份解析与 provision(workspace / project / actor)

**Files:**
- Create: `internal/memorylake/identity.go`, `internal/memorylake/identity_test.go`

**Interfaces:**
- Consumes: `Client`(Task 4)、`Config`
- Produces:
  - `func (c *Client) ResolveWorkspaceID(customIDOrID string) (string, error)`(name/custom_id 匹配 `GET /api/v3/workspaces`;`ws-` 前缀直返)
  - `func (c *Client) EnsureProject(ws, name string) (projID string, err error)`(list 找 custom_id==name,无则 `POST .../projects`)
  - `func (c *Client) EnsureActor(ws, customID, displayName string) (actorID string, err error)`(建 actor + 绑定 workspace,幂等)

- [ ] **Step 1: 写 EnsureProject 测试(httptest,先失败)**

```go
// internal/memorylake/identity_test.go —— 用 httptest mock:
// GET  /api/v3/workspaces/{ws}/projects → 首次空列表
// POST /api/v3/workspaces/{ws}/projects → 返回 {id:"proj-new"}
// 断言 EnsureProject 返回 "proj-new";第二次 list 命中已存在则不再 POST。
```

> 实施者注:mock server 用 `r.Method + r.URL.Path` 分派;维护一个内存 slice 模拟 project 列表,POST 后加入,GET 返回之。断言两次 EnsureProject 幂等(第二次不 POST,可用计数器校验)。

- [ ] **Step 2: 运行,确认失败**

Run: `go test ./internal/memorylake/ -run TestEnsureProject`
Expected: FAIL —— `undefined`

- [ ] **Step 3: 实现 identity.go**

```go
// internal/memorylake/identity.go
package memorylake
import "strings"

type wsItem struct{ ID, Name, CustomID string `json:"custom_id"` }

func (c *Client) ResolveWorkspaceID(x string) (string, error) {
	if strings.HasPrefix(x, "ws-") { return x, nil }
	var out struct{ Items []wsItem `json:"items"` }
	if err := c.doJSON("GET", "/api/v3/workspaces", nil, &out); err != nil { return "", err }
	for _, w := range out.Items {
		if w.CustomID == x || w.Name == x { return w.ID, nil }
	}
	return "", &APIError{Code: "NOT_FOUND", Message: "workspace not found: " + x}
}

type projItem struct{ ID, Name, CustomID string `json:"custom_id"` }

func (c *Client) EnsureProject(ws, name string) (string, error) {
	var out struct{ Items []projItem `json:"items"` }
	if err := c.doJSON("GET", "/api/v3/workspaces/"+ws+"/projects", nil, &out); err != nil { return "", err }
	for _, p := range out.Items {
		if p.CustomID == name || p.Name == name { return p.ID, nil }
	}
	var created struct{ ID string `json:"id"` }
	body := map[string]any{"custom_id": name, "name": name}
	if err := c.doJSON("POST", "/api/v3/workspaces/"+ws+"/projects", body, &created); err != nil { return "", err }
	return created.ID, nil
}

func (c *Client) EnsureActor(ws, customID, displayName string) (string, error) {
	var created struct{ ID string `json:"id"` }
	body := map[string]any{"custom_id": customID, "actor_type": "HUMAN", "display_name": displayName}
	if err := c.doJSON("POST", "/api/v3/actors", body, &created); err != nil {
		// 已存在时按需 list 查回;首版可容忍幂等错误码后再 list(实施期按真实错误码补全)
		return "", err
	}
	// 绑定 workspace(幂等)
	_ = c.doJSON("POST", "/api/v3/workspaces/"+ws+"/actors", map[string]any{"actor_id": created.ID}, nil)
	return created.ID, nil
}
```

> 实施者注(spike):真实"actor 已存在"的错误码需实测补全(见 spec §11.5);首版可先假设每机唯一 custom_id。分页:workspace/project list 可能是 cursor 式(`continuation_token`),数据量大时需翻页,首版可只取首页并留 TODO。

- [ ] **Step 4: 运行,确认通过** — `go test ./internal/memorylake/ -run TestEnsureProject` → PASS
- [ ] **Step 5: Commit**

```bash
git add internal/memorylake/identity.go internal/memorylake/identity_test.go
git commit -m "feat(memorylake): workspace/project/actor resolution and provisioning"
```

---

## Task 6: id 映射 + Observation⇄fact 映射

**Files:**
- Create: `internal/memorylake/idmap.go`, `internal/memorylake/mapper.go`, `internal/memorylake/mapper_test.go`

**Interfaces:**
- Produces:
  - `type IDMap struct{...}`;`Load/Save`;`func (*IDMap) IntFor(factID string) int64`(不存在则分配自增)、`func (*IDMap) FactFor(id int64) (string, bool)`
  - `func FactMetadata(p store.AddObservationParams, obsID string, raw string) map[string]any`(编码 engram 字段)
  - `func ObservationFromFact(f Fact) store.Observation`(解码;`content` 取 `metadata.engram_raw`,回退 `f.Fact`)
  - `type Fact struct { ID, Fact string; Metadata map[string]any; Score float64; CreatedAt, UpdatedAt string; Expired bool }`

- [ ] **Step 1: 写 mapper round-trip 测试(先失败)**

```go
// internal/memorylake/mapper_test.go
package memorylake
import ("testing"; "github.com/[org]/engram/internal/store")

func TestObservationFromFactPrefersRaw(t *testing.T) {
	f := Fact{ID: "fact-1", Fact: "paraphrased text",
		Metadata: map[string]any{"engram_raw": "EXACT original", "engram_type": "decision", "engram_scope": "project", "topic_key": "arch/db"}}
	obs := ObservationFromFact(f)
	if obs.Content != "EXACT original" { t.Fatalf("content=%q, want raw", obs.Content) }
	if obs.Type != "decision" || obs.Scope != "project" || obs.TopicKey != "arch/db" {
		t.Fatalf("metadata not decoded: %+v", obs)
	}
}
func TestFactMetadataCarriesRaw(t *testing.T) {
	md := FactMetadata(store.AddObservationParams{Title: "T", Content: "C", Type: "bugfix", Scope: "project", TopicKey: "x/y"}, "obs-9", "C")
	if md["engram_raw"] != "C" || md["engram_type"] != "bugfix" { t.Fatalf("bad metadata: %+v", md) }
}
```

> 实施者注:`store.Observation` / `store.AddObservationParams` 的真实字段名(`Content`/`Type`/`Scope`/`TopicKey`/`Title`)以 `internal/store/store.go:88` 附近 struct 为准,不符则据实改断言与实现。

- [ ] **Step 2: 运行,确认失败** — `go test ./internal/memorylake/ -run "TestObservationFromFact|TestFactMetadata"` → FAIL
- [ ] **Step 3: 实现 idmap.go + mapper.go**

```go
// internal/memorylake/idmap.go
package memorylake
import ("encoding/json"; "os"; "path/filepath"; "sync")

type IDMap struct {
	mu       sync.Mutex
	Next     int64            `json:"next"`
	IntByFact map[string]int64 `json:"int_by_fact"`
	FactByInt map[string]string `json:"-"` // 反向,Load 时重建(键用 strconv)
	path     string
}
func LoadIDMap(path string) (*IDMap, error) {
	m := &IDMap{Next: 1, IntByFact: map[string]int64{}, FactByInt: map[string]string{}, path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) { return m, nil }
	if err != nil { return nil, err }
	if err := json.Unmarshal(b, m); err != nil { return nil, err }
	if m.IntByFact == nil { m.IntByFact = map[string]int64{} }
	if m.Next == 0 { m.Next = 1 }
	m.FactByInt = map[string]string{}
	for f, i := range m.IntByFact { m.FactByInt[itoa(i)] = f }
	m.path = path
	return m, nil
}
func (m *IDMap) IntFor(factID string) int64 {
	m.mu.Lock(); defer m.mu.Unlock()
	if i, ok := m.IntByFact[factID]; ok { return i }
	i := m.Next; m.Next++
	m.IntByFact[factID] = i; m.FactByInt[itoa(i)] = factID
	_ = m.save()
	return i
}
func (m *IDMap) FactFor(id int64) (string, bool) {
	m.mu.Lock(); defer m.mu.Unlock()
	f, ok := m.FactByInt[itoa(id)]; return f, ok
}
func (m *IDMap) save() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil { return err }
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(m.path, b, 0o644)
}
func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }
```

```go
// internal/memorylake/mapper.go
package memorylake
import "github.com/[org]/engram/internal/store"

type Fact struct {
	ID        string         `json:"id"`
	Fact      string         `json:"fact"`
	Metadata  map[string]any `json:"metadata"`
	Score     float64        `json:"score"`
	Expired   bool           `json:"expired"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

func FactMetadata(p store.AddObservationParams, obsID, raw string) map[string]any {
	md := map[string]any{
		"engram_raw":   raw,
		"engram_title": p.Title,
		"engram_type":  p.Type,
		"engram_scope": p.Scope,
		"engram_obs_id": obsID,
	}
	if p.TopicKey != "" { md["topic_key"] = p.TopicKey }
	return md
}

func ObservationFromFact(f Fact) store.Observation {
	get := func(k string) string { if v, ok := f.Metadata[k].(string); ok { return v }; return "" }
	content := get("engram_raw")
	if content == "" { content = f.Fact }
	return store.Observation{
		Content:  content,
		Title:    get("engram_title"),
		Type:     get("engram_type"),
		Scope:    get("engram_scope"),
		TopicKey: get("topic_key"),
		// ID/时间戳由 backend 层填充(需 IDMap + 解析 CreatedAt)
	}
}
```

- [ ] **Step 4: 运行,确认通过** — 同 Step 2 命令 → PASS
- [ ] **Step 5: Commit**

```bash
git add internal/memorylake/idmap.go internal/memorylake/mapper.go internal/memorylake/mapper_test.go
git commit -m "feat(memorylake): int64<->fact-id map and Observation<->fact mapper (raw-preferring)"
```

---

## Task 7: 异步写入(append → 轮询抽取 → PATCH 回填)

**Files:**
- Create: `internal/memorylake/writequeue.go`, `internal/memorylake/writequeue_test.go`

**Interfaces:**
- Consumes: `Client`、`IDMap`、`FactMetadata`
- Produces:
  - `func (c *Client) AppendObservation(ws, projID, convCustomID, actorID string, p store.AddObservationParams) (msgID string, err error)`(必要时建 conversation)
  - `func (c *Client) BackfillFacts(ws, projID, sinceMsg string, md map[string]any, poll, maxWait time.Duration) ([]Fact, error)`(轮询新 fact 并 PATCH 回填 metadata)

- [ ] **Step 1: 写 backfill 测试(httptest,先失败)**

```go
// mock:GET .../facts 前 2 次空,第 3 次返回 1 条新 fact;PATCH .../facts/{id} 记录收到的 metadata。
// 断言 BackfillFacts 在 maxWait 内返回该 fact,且对它发过一次带 engram_raw 的 PATCH。
```

- [ ] **Step 2: 运行,确认失败** — `go test ./internal/memorylake/ -run TestBackfill` → FAIL
- [ ] **Step 3: 实现 writequeue.go**（append 建会话/发消息;backfill 轮询 list + 逐条 PATCH 回填;超时返回已得部分）

```go
// 关键点(实施者按 §5.1 流程写全):
// AppendObservation:
//   1) ensure conversation(custom_id=convCustomID, kind=DIRECT, actor_ids, rw_project_ids=[projID])
//   2) POST /api/v3/conversations/{conv}/messages {custom_id: hash(content), actor_id, content:[{block_type:"TEXT", text: p.Content}]}
//   3) 返回 message id
// BackfillFacts:
//   for elapsed < maxWait { list facts;取 CreatedAt 晚于起始时刻的新 fact;
//     对每条尚未回填(metadata 无 engram_obs_id)的 fact PATCH md;收集;若已≥1 条且稳定则返回 }
```

> 完整代码在实现时展开;轮询用 `time.NewTicker(poll)` + `time.After(maxWait)`;PATCH body `{"metadata": md}`。custom_id 用 `content` 的 sha256 前 16 hex,保证幂等。

- [ ] **Step 4: 运行,确认通过** — `go test ./internal/memorylake/ -run TestBackfill` → PASS
- [ ] **Step 5: Commit**

```bash
git add internal/memorylake/writequeue.go internal/memorylake/writequeue_test.go
git commit -m "feat(memorylake): async append + poll-extract + metadata backfill"
```

---

## Task 8: 检索(语义 + fact_fuzzy 合并 + 客户端过滤)

**Files:**
- Create: `internal/memorylake/search.go`, `internal/memorylake/search_test.go`

**Interfaces:**
- Consumes: `Client`、`Fact`、`ObservationFromFact`
- Produces: `func (c *Client) SearchFacts(ws, projID, actorID, query string, opts store.SearchOptions) ([]store.SearchResult, error)`

- [ ] **Step 1: 写检索测试(httptest,先失败)** —— mock `POST .../memories/search` 返回带 score+metadata 的 facts;断言:结果 `content` 取自 `engram_raw`;按 `opts.Type` 过滤;`rank` 来自 score;query 含 `/` 时额外发 `GET .../facts?fact_fuzzy=` 并置顶。
- [ ] **Step 2: 运行,确认失败** — `go test ./internal/memorylake/ -run TestSearchFacts` → FAIL
- [ ] **Step 3: 实现 search.go**（语义搜索主路径 + 客户端按 metadata 过滤 type/scope + topic_key `/` 走 fuzzy 置顶,合并去重,`SearchResult.Rank = score`）
- [ ] **Step 4: 运行,确认通过** — → PASS
- [ ] **Step 5: Commit**

```bash
git add internal/memorylake/search.go internal/memorylake/search_test.go
git commit -m "feat(memorylake): semantic + fuzzy search with client-side type/scope filter"
```

---

## Task 9: `MemoryLakeBackend` 实现 `MemoryBackend`

**Files:**
- Create: `internal/memorylake/backend.go`, `internal/memorylake/backend_test.go`

**Interfaces:**
- Consumes: 全部 Task 3–8
- Produces: `func NewBackend(cfg Config, ws, projID string) (*MemoryLakeBackend, error)`;`*MemoryLakeBackend` 实现 `mcp.MemoryBackend`

> 注意包依赖方向:`internal/memorylake` 不能 import `internal/mcp`(会环)。做法:接口在 `internal/mcp` 定义,断言放在 `internal/mcp`(Task 10)用 blank import 校验;`MemoryLakeBackend` 只需方法齐全即可被赋值。

- [ ] **Step 1: 写 backend 行为测试(httptest 全链路,先失败)**：`AddObservation` 返回 int64(经 IDMap)、`GetObservation(id)` 经 IDMap→fact→Observation、`Search` 走 Task 8、`DeleteObservation` 调 forget、`MergeProjects` 返回"不支持"错误、`PinObservation` 走 PATCH metadata.pinned。
- [ ] **Step 2: 运行,确认失败**
- [ ] **Step 3: 实现 backend.go** —— 逐方法映射(见 spec §6 表)。要点:
  - `AddObservation`:AppendObservation → 起 goroutine BackfillFacts(异步);立即返回一个占位 int64(基于 message hash 稳定分配)——或同步等首条 fact(依 `ENGRAM_BACKEND_SYNC` 逃生舱)。**默认异步**:返回占位 id,后台回填后 IDMap 补映射。
  - `GetObservation/UpdateObservation/DeleteObservation`:IDMap→fact-id→对应 V3 端点。
  - `MergeProjects`:`return nil, errors.New("MemoryLake 后端不支持项目合并")`。
  - `PinObservation/UnpinObservation`:读 fact→合并 metadata→PATCH。
  - `JudgeRelation/JudgeBySemantic/FindCandidates/GetRelationsForObservations`:映射到 V3 conflicts(list/resolve);首版可先返回空/记录并加 TODO(见 spec §6.1 近似映射)。
  - `Stats/CountObservationsForProject`:list 分页 total / statistics 端点。
  - `Timeline`:按 created_at 排序取锚点前后 N。
  - `ObservationsNeedingReview/MarkReviewed`:客户端按 type 衰减 + fact expiration_date。
- [ ] **Step 4: 运行,确认通过**
- [ ] **Step 5: Commit**

```bash
git add internal/memorylake/backend.go internal/memorylake/backend_test.go
git commit -m "feat(memorylake): MemoryLakeBackend implementing MemoryBackend over V3 facts"
```

---

## Task 10: 逐 project 路由接线

**Files:**
- Modify: `internal/mcp/mcp.go`(或 `cmd/engram` 组装 selector 处)、`internal/mcp/selector.go`
- Create: `internal/mcp/selector_route_test.go`

**Interfaces:**
- Consumes: `Enablement`、`memorylake.NewBackend`、`StaticSelector`
- Produces: `func NewRoutingSelector(sqlite MemoryBackend, cfg memorylake.Config, enab *memorylake.Enablement) BackendSelector`

- [ ] **Step 1: 写路由测试(先失败)**：enabled project → 返回 MemoryLake 后端(可用 mock factory 注入);未 enabled → 返回 sqlite;`ENGRAM_BACKEND=sqlite` → 恒 sqlite。
- [ ] **Step 2: 运行,确认失败**
- [ ] **Step 3: 实现 `NewRoutingSelector`**：闭包内查 `enab.IsEnabled(project)`;命中则惰性 `memorylake.NewBackend(cfg, ws, projID)`(带缓存,避免每次调用重建);全局 `ENGRAM_BACKEND=sqlite` 短路。在 `cmd/engram` 启动 MCP server 处用它替换默认 `StaticSelector`。
- [ ] **Step 4: 运行,确认通过 + 端到端构建** — `go build ./cmd/engram && go test ./...`
- [ ] **Step 5: Commit**

```bash
git add internal/mcp/selector.go internal/mcp/selector_route_test.go internal/mcp/mcp.go cmd/engram/main.go
git commit -m "feat(mcp): route enabled projects to MemoryLake, default SQLite"
```

---

## Task 11: 文档 + 可选 e2e

**Files:**
- Modify: `DOCS.md`、`CLAUDE.md`(配置项、后端选择、限制)
- Create(可选): `internal/memorylake/e2e_test.go`(`//go:build e2e`)

- [ ] **Step 1: 更新 `DOCS.md`**:新增"MemoryLake 后端"节 —— 环境变量表、`engram memorylake enable/disable/status`、逐 project 语义、已知限制(异步~12s、抽取改写靠 engram_raw 读回、无硬删、merge 不支持)。
- [ ] **Step 2: 更新 `CLAUDE.md`**:在"Interface & memory-model gotchas"补一行 MemoryLake 后端为逐 project opt-in、默认 SQLite。
- [ ] **Step 3(可选): e2e**:`//go:build e2e`,对真实 `engram` workspace 建临时 selftest project 跑 save→轮询→search→get→forget→delete project(用后即删),校验 metadata round-trip 与抽取延迟。
- [ ] **Step 4: 运行 `go test ./...` + `go build ./cmd/engram`,确认全绿**
- [ ] **Step 5: Commit**

```bash
git add DOCS.md CLAUDE.md internal/memorylake/e2e_test.go
git commit -m "docs(memorylake): document per-project backend, config, limits"
```

---

## Self-Review(对照 spec)

- **覆盖**:身份/配置(T3,T5)、写路径(T7,T9)、读路径(T8,T9)、逐 project 选择(T2,T3,T10)、原文保真(T6)、id 模型(T6)、mem_* 映射表(T9 逐方法)、不可映射项 merge/doctor(T9)、错误/异步(T4,T7,T9)、迁移开关(T3,T10)、测试(各 Task + T11)。冲突/judge 首版为近似映射,已在 T9 标 TODO 对齐 spec §6.1/§11。
- **占位符**:新组件(T1,T3,T4,T5,T6)含完整代码;T7/T8/T9 因体量给出精确接口 + 关键实现骨架 + 逐点规格(实现时展开),属"复杂映射按规格展开",非 TBD。
- **类型一致**:接口货币类型统一 `store.*`;`Fact`/`store.Observation` 字段名以 `internal/store` 真实 struct 为准(各 Task 已注明核对点)。`[org]/engram` module path 各处统一替换为 `go.mod` 真实值。

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-22-memorylake-backend.md`.
