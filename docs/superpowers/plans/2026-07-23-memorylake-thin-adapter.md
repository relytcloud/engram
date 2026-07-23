# Engram × MemoryLake 薄适配器(Option A + A1′)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Engram-on-MemoryLake 瘦身为薄适配器:去重/更新/冲突/内容管理下沉给 mem0;by-id 工具改用已有的 `sync_id` 字符串键(删除 idmap);MemoryLake 路径纯语义检索、最小工具面。**未 enable 的 SQLite 项目逻辑不变。**

**Architecture:** 两阶段。3A:把 `MemoryBackend` 接口 by-id 方法的键从 `int64` 改为 `string`(= `sync_id`),`*store.Store` 用薄适配器 `sqliteBackend` 经现有 `GetObservationBySyncID` 转译;MemoryLake 令 `SyncID=fact-id`。3B:删 idmap/topicindex/dedup/backfill-wait/engram_raw-preference/fact_fuzzy,`mem_judge`/`mem_compare` 改读 conflict API,`mem_save` append 即返回,精简 agent 协议。

**Tech Stack:** Go(CGO_ENABLED=0),`internal/mcp`(接口+handler)、`internal/memorylake`、`internal/store`(只读用其 `GetObservationBySyncID`)、`internal/server`/`internal/tui`/`cmd/engram`(by-id 放宽)、`plugin/*`、`internal/cloud/dashboard`。

## Global Constraints
- `CGO_ENABLED=0`;`go build ./...`、`go test ./...`、`go test -race ./internal/mcp/ ./internal/memorylake/ ./cmd/engram/ ./internal/server/` 全绿。
- **未 enable 的 project(SQLite)逻辑行为不变**;唯一对外变化:by-id 工具的 `id` 键由 int64 变字符串(值=sync_id),这对两后端一致。
- **不改 `store.Observation.ID int64`,不碰 `internal/cloud` 同步数据模型。**
- 允许改 `plugin/*` 与 `internal/cloud/dashboard`(仅 by-id id 解析放宽)。
- Conventional commit;**禁止 `Co-Authored-By`**。禁改 `./memorylake/`(上游参考仓库)。
- Module `github.com/Gentleman-Programming/engram`。
- 权威规格:`docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md`。

---

# 阶段 3A — by-id 键统一为 sync_id 字符串(SQLite 全绿优先)

## Task 1: `MemoryBackend` 接口 by-id 方法改用 string(sync_id)+ sqliteBackend 适配器

**Files:**
- Modify: `internal/mcp/backend.go`(接口方法签名 int64→string)
- Create: `internal/mcp/sqlite_backend.go`(`sqliteBackend` 适配器)
- Test: `internal/mcp/sqlite_backend_test.go`

**Interfaces:**
- Produces:
  - 接口 by-id 方法改为:`GetObservation(syncID string)`、`UpdateObservation(syncID string, p store.UpdateObservationParams)`、`DeleteObservation(syncID string, hardDelete bool)`、`PinObservation(syncID string)`、`UnpinObservation(syncID string)`、`Timeline(syncID string, before, after int)`、`MarkReviewed(syncID string)`、`FindCandidates(savedSyncID string, opts store.CandidateOptions)`;`AddObservation` 返回 `(string, error)`(返回 sync_id);`AddPrompt`/`AddPromptIfMissing` 返回 `(string, …)`。
  - `type sqliteBackend struct{ s *store.Store }`,实现新接口:每个 by-id 方法先 `obs, err := b.s.GetObservationBySyncID(syncID)`(store.go:4317)拿到 `obs.ID int64`,再调既有 `b.s.XxxObservation(obs.ID, …)`;`AddObservation` 调 `b.s.AddObservation(p)` 得 int64 后返回该行的 `sync_id`(用 `GetObservation(int64)` 取 `.SyncID`)。
  - `func newSQLiteBackend(s *store.Store) *sqliteBackend`。

- [ ] **Step 1: 写适配器测试(先失败)** —— 用真实临时 `*store.Store`:AddObservation 返回非空 sync_id;GetObservation(该 sync_id) 取回同一条;Delete/Pin by sync_id 生效;未知 sync_id 返回 not-found。
- [ ] **Step 2: 运行确认失败**(`sqliteBackend` 未定义)。
- [ ] **Step 3: 改 `backend.go` 接口签名**(上述 by-id 方法 int64→string);**删除** `var _ MemoryBackend = (*store.Store)(nil)`(不再直接实现)。
- [ ] **Step 4: 实现 `sqlite_backend.go`**(转译:sync_id→GetObservationBySyncID→int64→既有方法;非 by-id 方法如 Search/Stats/Sessions 直接委托 `b.s`)。加 `var _ MemoryBackend = (*sqliteBackend)(nil)`。
- [ ] **Step 5: 运行 `go build ./internal/mcp/`**(会因 mcp.go handler 仍用 int64 而失败 → 由 Task 2 修;本 task 先让 backend.go + 适配器自洽,可临时用 `go vet ./internal/mcp/backend.go` 或先跳过 handler)。
  > 注:Task 1、2 强耦合,建议合并为一个 review 单元实现——见 Task 2。
- [ ] **Step 6: Commit** `refactor(mcp): key MemoryBackend by sync_id string; add sqliteBackend adapter`

## Task 2: handler + 工具 schema 改用 sync_id;装配换成 sqliteBackend

**Files:**
- Modify: `internal/mcp/mcp.go`(by-id handler 读 string;工具 `id` `WithNumber`→`WithString`;`selector`/构造函数注入 `newSQLiteBackend(s)`)
- Modify: `internal/mcp/selector.go`(`StaticSelector`/`NewRoutingSelector` 返回的默认后端包成 `sqliteBackend`)
- Test: 更新 `internal/mcp/*_test.go` 中断言 int64 id 的用例

- [ ] **Step 1**:把 by-id handler 里 `id := int64(intArg(req,"id",0))` 改为 `id, _ := req.GetArguments()["id"].(string)`(mem_get/update/delete/pin/unpin/timeline/review/compare;`observation_id` 同理);校验非空。
- [ ] **Step 2**:工具定义处 `mcp.WithNumber("id"…)`/`WithNumber("observation_id"…)`/`WithNumber("memory_id_a/b"…)` → `mcp.WithString(…)`,描述改为"observation 的 sync_id(来自 search/save 结果)"。
- [ ] **Step 3**:所有构造/selector 处把注入的 `*store.Store` 包成 `newSQLiteBackend(s)`(`StaticSelector`、`NewRoutingSelector` 的 sqlite 分支、`DoctorToolHandler` 等)。
- [ ] **Step 4**:更新既有测试 —— 凡断言/传入 int64 id 的,改为用 sync_id 字符串(从 save/search 结果取 `sync_id`)。**断言语义不变,只换 id 表示**。
- [ ] **Step 5**:`go build ./cmd/engram && go test ./internal/mcp/`(全绿)。
- [ ] **Step 6: Commit** `refactor(mcp): by-id tools accept sync_id string, wire sqliteBackend`

## Task 3: HTTP server / TUI / CLI by-id 放宽为 sync_id 字符串

**Files:**
- Modify: `internal/server/server.go`(by-id 端点:`/observations/{id}` 等改用 sync_id;`backendFor*` 已是接口)
- Modify: `internal/tui/*.go`(命令里传 observation id 处改用 sync_id;注意 TUI 的 `*store.Store` 直用方法不在接口的,仍走 store 但键用 sync_id)
- Modify: `cmd/engram/main.go`(`cmdSave`/`cmdSearch` 及任何 by-id CLI)
- Test: `internal/server/*_test.go`、`cmd/engram/*_test.go` 相应更新

- [ ] **Step 1**:server by-id handler 从路径/参数取字符串 id → 经 `backendForX` 调新接口(string 键);移除 int 解析。
- [ ] **Step 2**:TUI 里选中项用 `obs.SyncID` 作为后续 get/pin/delete 的键。
- [ ] **Step 3**:CLI by-id 参数放宽为字符串。
- [ ] **Step 4**:更新测试;`go test ./... && go test -tags e2e ./internal/server/...` 全绿。
- [ ] **Step 5: Commit** `refactor(server,tui,cli): key observation ops by sync_id string`

## Task 4: plugin / dashboard 的 observation id 解析放宽

**Files:**
- Modify: `plugin/*`(先 `grep -rn "observation" plugin/ | grep -iE "parseInt|Number|id"` 定位按数字解析 observation id 的地方)
- Modify: `internal/cloud/dashboard/*`(同上,dashboard 若按 int 渲染/链接 observation id)

- [ ] **Step 1**:grep 定位 plugin/dashboard 里对 observation id 的数字假设(`parseInt`/`Number(`/`%d`/int 路由参数)。
- [ ] **Step 2**:逐处放宽为字符串(sync_id);若某 plugin 根本不 by-id 操作 observation,则无需改,在报告说明。
- [ ] **Step 3**:构建/相应测试;`make templ` 若改了 dashboard `.templ` 则重生成并提交 `*_templ.go`。
- [ ] **Step 4: Commit** `refactor(plugin,dashboard): accept string observation id (sync_id)`

## Task 5(3A 收尾): 全量回归 + 文档

- [ ] `go build ./...`、`go test ./...`、`go test -race ./internal/mcp/ ./cmd/engram/ ./internal/server/`、`go test -tags e2e ./internal/server/...` 全绿。
- [ ] 更新 `DOCS.md`/`CLAUDE.md`:observation by-id 键为 sync_id 字符串(两后端一致)。
- [ ] **Commit** `docs: observation by-id key is now sync_id string`

> **3A 完成即可独立合并/验证**:SQLite 逻辑不变,只是 by-id 键从 int64 变 sync_id。MemoryLake 路径此时仍用 idmap(Task 6 才删)——为兼容,MemoryLakeBackend 的新 string 方法此阶段可暂时内部 `idmap.FactFor` 兼容,3B 再简化。

---

# 阶段 3B — MemoryLake 能力下沉给 mem0

## Task 6: 删 idmap/topicindex,MemoryLakeBackend by-id 直用 fact-id

**Files:**
- Delete: `internal/memorylake/idmap.go`、`internal/memorylake/idmap_test.go`、`internal/memorylake/topicindex.go`、`internal/memorylake/topicindex_test.go`
- Modify: `internal/memorylake/backend.go`(by-id 方法:sync_id **就是** fact-id,直接调 MemoryLake;删 idmap/topics 字段与用法;`ObservationFromFact` 令 `SyncID=fact.ID`)
- Modify: `internal/memorylake/mapper.go`(`ObservationFromFact` 设 `SyncID`;`content` 改为返回 `fact.Fact`(不再优先 engram_raw))
- Modify: `cmd/engram/routing.go`(`NewBackend` 不再需要 idmap 参数)
- Test: `internal/memorylake/backend_test.go` 更新

- [ ] **Step 1**:测试更新 —— GetObservation(fact-id) 直取、返回 Observation.SyncID==fact-id、content==fact 文本;删除 idmap/topics 相关测试。
- [ ] **Step 2**:改 backend.go by-id 方法直接用 sync_id=fact-id 调 `getFact`/PATCH/forget;删 `factForID`/`idmap`/`topics` 字段;`NewBackend` 签名去掉 idmap。
- [ ] **Step 3**:删四个文件;改 routing.go 装配。
- [ ] **Step 4**:`go build ./... && go test ./internal/memorylake/ ./cmd/engram/`(全绿)。
- [ ] **Step 5: Commit** `refactor(memorylake): use fact-id as sync_id, drop idmap and topic index`

## Task 7: mem_save 去掉同步回填与去重/upsert(下沉给 mem0)

**Files:**
- Modify: `internal/memorylake/backend.go`(`AddObservation`:append 消息即返回 fact/pending sync_id;删 pre-append 快照、BackfillFacts 等待、hash 去重、topic_key upsert 分支)
- Modify: `internal/memorylake/writequeue.go`(删 `BackfillFacts`;`AppendObservation` 保留;`listFacts` 仅搜索用)
- Modify: `internal/mcp/mcp.go`(`handleSave`:MemoryLake 路径不生成/返回 candidates)
- Test: 更新

- [ ] **Step 1**:测试 —— MemoryLake AddObservation 只 append 消息即返回(不轮询),快;不再有 engram_raw PATCH;mem_save 响应不含 `judgment_required`。
- [ ] **Step 2**:实现:`AddObservation` = ensureConversation + AppendObservation → 返回一个 sync_id(优先:短等一拍拿首个 fact-id;拿不到则返回 message 引用作 pending sync_id)。删 backfill/dedup/topic 分支。
- [ ] **Step 3**:`handleSave` 对 MemoryLake 后端跳过候选生成(SQLite 保留)。
- [ ] **Step 4**:`go test ./internal/memorylake/ ./internal/mcp/`(全绿)。
- [ ] **Step 5: Commit** `refactor(memorylake): mem_save appends and returns without sync backfill; defer dedup/upsert to mem0`

## Task 8: mem_judge/mem_compare 改读 conflict API;FindCandidates 不再本地生成

**Files:**
- Modify: `internal/memorylake/conflict.go`、`internal/memorylake/backend.go`(`FindCandidates` 返回空或读 conflict list;`JudgeBySemantic`/关系判定映射到 conflict resolve;保留已实现的 conflict.go 逻辑)
- Test: 更新

- [ ] **Step 1**:测试 —— MemoryLake FindCandidates 返回空(不再 FTS 候选);mem_judge/compare 走 conflict API(mock)。
- [ ] **Step 2**:实现(conflict.go 已有 list/get/resolve 映射,确保 by sync_id=fact-id;`FindCandidates` 不再生成候选)。
- [ ] **Step 3**:`go test ./internal/memorylake/`(全绿)。
- [ ] **Step 4: Commit** `refactor(memorylake): defer conflict handling to mem0; judge/compare via conflict API only`

## Task 9: 纯语义检索(删 fact_fuzzy)

**Files:**
- Modify: `internal/memorylake/search.go`(删 `fuzzyFacts` 与 `/` 触发的 fuzzy 置顶;`SearchFacts` 仅语义 + 客户端 type/scope 过滤 + SyncID=fact-id)
- Test: 更新

- [ ] **Step 1**:测试 —— query 含 `/` 不再发 fact_fuzzy;结果仅语义;SyncID==fact-id。
- [ ] **Step 2**:删 fuzzy 路径。
- [ ] **Step 3**:`go test ./internal/memorylake/`(全绿)。
- [ ] **Step 4: Commit** `refactor(memorylake): pure semantic search, drop fact_fuzzy keyword augmentation`

## Task 10: 精简 agent 协议(plugin) + 文档

**Files:**
- Modify: `plugin/*` 里注入 memory-protocol 文案的地方(SessionStart hook / MCP server instructions)
- Modify: `DOCS.md`

- [ ] **Step 1**:协议文案加一句:"MemoryLake 后端的 project:去重/更新/矛盾合并由后端自动完成,只需 `mem_save`/`mem_search`/`mem_context`,不必手动 `mem_update`/`mem_judge`/`mem_compare`。"
- [ ] **Step 2**:`DOCS.md` 更新薄适配器语义(mem_save 异步秒回、内容返回 mem0 fact 文本、纯语义、conflict 交 mem0、最小工具面)。
- [ ] **Step 3: Commit** `docs(memorylake): thin-adapter semantics and simplified agent protocol`

## Task 11: parity/差分测试更新 + 全量回归

**Files:**
- Modify: `internal/paritytest/*`(EXACT 不再比逐字 → 比"语义等价/key_facts 保留";加"mem_save 秒回""by-id via sync_id"等;临时项目清理)
- 验证

- [ ] **Step 1**:更新 parity 用例到薄适配器语义。
- [ ] **Step 2**:`go build -tags parity ./internal/paritytest/`;可选带 key 实跑一轮。
- [ ] **Step 3**:全量 `go build ./...`、`go test ./...`、`go test -race …`、`-tags e2e` 全绿。
- [ ] **Step 4: Commit** `test(parity): update differential harness for thin-adapter semantics`

---

## Self-Review(对照 spec)
- **覆盖**:下沉去重/更新/冲突(T7/T8)、删 idmap+id=sync_id(T1/T6)、纯语义(T9)、最小工具面+协议(T2/T10)、id 契约放宽含 plugin/dashboard(T1-4)、mem_save 秒回(T7)、parity(T11)、SQLite 不变(T1-3 断言等价)。
- **分阶段**:3A(T1-5)独立可合并、SQLite 全绿;3B(T6-11)MemoryLake 下沉。符合 spec §9 建议。
- **占位符**:较大删除/重构任务给了精确文件 + 关键签名 + store 现成 `GetObservationBySyncID` 支撑;实现时按此展开。
- **类型一致**:接口 by-id 统一 string(sync_id);MemoryLake SyncID=fact-id;SQLite 经适配器转译。`[org]/engram` 已用真实 module。

## Execution Handoff
Plan complete and saved to `docs/superpowers/plans/2026-07-23-memorylake-thin-adapter.md`.
