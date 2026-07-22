# MemoryLake 后端 Phase 2 — 完成所有功能

> 承接 `2026-07-22-memorylake-backend.md`(Phase 1,已合并到 PR #1)。Phase 1 交付了 opt-in scaffolding:核心 CRUD+Search+原文保真做实,其余为 fail-safe first-cut。Phase 2 把 first-cut 补实、消除跨接口脑裂、补分页/去重、实跑 parity。

**Global Constraints(同 Phase 1)**
- `CGO_ENABLED=0` 纯 Go;`go build ./...`、`go test ./...`、`go test -race ./cmd/engram/ ./internal/memorylake/` 全绿。
- 只改 engram 服务;不改 MCP 工具对外契约;**未 enable 的 project 行为逐字不变**。
- 禁改仓库根 `./memorylake/`;Conventional commit;**禁止 `Co-Authored-By`**。
- Module `github.com/Gentleman-Programming/engram`。`internal/memorylake` ≠ `./memorylake/`。

**已实测的 API 事实(实现依据)**
- actor 重复 custom_id → `error_code:"CUSTOM_ID_CONFLICT"`;`GET /api/v3/actors?page_size=N`(游标 continuation_token)返回 items 含 `id/custom_id`,可按 custom_id 找回。
- V3 conflict:`GET /api/v3/workspaces/{ws}/memories/conflicts?project_id=<proj>&page_size=N`(project 范围,project_id 必填);另有 V2 `GET/POST /api/v2/projects/{proj}/memories/conflicts[...]/resolve`。
- 列表分页统一 `continuation_token` 游标(`LoadMoreResponse{items, continuation_token}`)。
- fact 只能按 `fact_fuzzy`(正文子串)过滤,**不能按 metadata 查** → 按 topic_key 找 fact 需**本地索引**。

---

## Task 12: topic_key upsert + read/aggregate 方法做实

**Files:** `internal/memorylake/backend.go`、新增 `internal/memorylake/topicindex.go`(topic_key→fact_id 本地索引,仿 idmap)、`internal/memorylake/backend_test.go`

- **topic_key upsert(核心)**:新增本地索引 `~/.engram/memorylake-topics-<projID>.json` 存 `project|scope|topic_key → fact_id`。`AddObservation` 当 `p.TopicKey != ""`:查索引命中 → 对该 fact `PATCH {fact: 新内容, metadata: {...engram_raw=新原文, engram_rev++}}` 原地更新(不 append 新消息),返回其 int64;未命中 → 正常 append 抽取,回填后把**首条** fact 记入 topic 索引。并发安全(mutex)。
- **ProjectExists(name)**:改为查 `GET /api/v3/workspaces/{ws}/projects`(或用已解析 projID 非空即存在),命中返回 true、否则 false —— 不再无条件 true。
- **ListProjectNames()**:列 workspace 下 projects 的 custom_id。
- **Stats() / CountObservationsForProject(name)**:用 list facts(分页累计)或 `GET /api/v3/workspaces/{ws}/projects/{proj}/statistics` 得计数,返回 `store.Stats`/int(排除 expired)。
- **Timeline(id,before,after)**:list facts 按 `created_at` 排序,定位锚点(idmap.FactFor(id)),取前后 N,组 `store.TimelineResult`。
- **FormatContext(project,scope)**:list facts + pinned(metadata.pinned)优先 + 近期,组人类可读串(参考 store.FormatContext 输出风格)。
- 测试:upsert 命中走 PATCH 不 append(计数器验证)、未命中记索引;ProjectExists 真值;Stats/Timeline/FormatContext 基本正确。

## Task 13: sessions + prompts + passive + review 做实

**Files:** `internal/memorylake/backend.go`（+其它同包辅助）、test

- **sessions ↔ conversations**:`CreateSession(id,project,dir)` ensure conversation(custom_id=id);`GetSession(id)` GET conversation→`*store.Session`;`EndSession(id,summary)` 记 conversation metadata(status=ended,summary);`MostRecentActiveSession(project)`/`RecentSessions(project,limit)` list conversations 排序映射。
- **prompts**:`AddPrompt`/`AddPromptIfMissing` 存为该 project 的一条 fact(type=`prompt`,或 conversation 消息);IfMissing 用 content-hash 去重(仿 message custom_id)。
- **PassiveCapture(p)**:复用 `store.ExtractLearnings`(纯函数解析 `## Key Learnings`)得条目 → 每条走 `AddObservation`(带 hash 去重)→ 返回 `store.PassiveCaptureResult{Extracted,Saved,Duplicates}`。
- **review**:`ObservationsNeedingReview(project,limit)` list facts,按 metadata.engram_type 的衰减策略(与 store 相同月数)对比 created_at 判过期;`MarkReviewed(id)` 重置(写 metadata.engram_review_after 或 fact expiration_date)。
- 测试:session 开-读-结、prompt 幂等、passive 解析计数、review 过期判定。

## Task 14: relations / conflict 映射

**Files:** `internal/memorylake/backend.go`、`internal/memorylake/conflict.go`、test

- **FindCandidates(savedID,opts)**:用 `GET .../conflicts?project_id=<proj>` 取与该 fact 相关冲突,映射为 `[]store.Candidate`;无则空。
- **GetRelationsForObservations(syncIDs)**:按 fact 的冲突信息组 `map[string]store.ObservationRelations`。
- **JudgeRelation(p)** / **JudgeBySemantic(p)**:映射到 conflict resolve —— `supersedes`→resolve keep_memory(新);`conflicts_with`→保留未决;`not_conflict`→忽略/标记;`related/compatible/scoped`→存关系 fact metadata 或客户端记录。返回 `*store.Relation`/sync_id。
- 无法一一对应处如实注释;保持 fail-safe。
- 测试:候选映射、判定 verb→resolve 策略映射、跨 project 守卫。

## Task 15: 跨接口路由(消除脑裂)

**Files:** `cmd/engram/main.go`(cmdSave/cmdSearch)、`internal/server`(HTTP)、`internal/tui`、复用 `NewRoutingSelector`、test

- 把 `cmdSave`、`cmdSearch`、`engram serve`(HTTP handlers)、`cmdTUI` 的存储访问从直接 `storeNew(cfg)` 改为经**同一 `NewRoutingSelector`** 逐 project 选后端。
- HTTP/TUI 需要接口化其 store 依赖(类似 Phase 1 对 mcp 的处理);**未 enable project 行为不变**。
- 谨慎:HTTP serve 供 OpenCode/Pi 会话跟踪,TUI 是只读为主;逐个接线并保回归测试全绿。
- 测试:enabled project 经 CLI/HTTP 也走 MemoryLake;非 enabled 不变。

## Task 16: 分页 + EnsureActor 去重

**Files:** `internal/memorylake/writequeue.go`、`identity.go`、`search.go`、test

- **分页**:`listFacts`、workspace/project list、actor list、search 结果 —— 用 `continuation_token` 循环取全量(设合理上限防失控),移除 first-page-only TODO。
- **EnsureActor 去重**:POST 返回 `CUSTOM_ID_CONFLICT` 时 → `GET /api/v3/actors`(分页)按 custom_id 找回其 id;再绑定 workspace(幂等)。消除重复 actor / 孤儿风险。
- 测试:多页 list 聚合;actor 重复走找回路径。

## Task 17: mem_stats / mem_doctor seam 接口化

**Files:** `internal/mcp/mcp.go`、test

- `mem_stats`:把 `loadMCPStats` 从 `*store.Store` 具体依赖改为经 `MemoryBackend.Stats()`,使 MemoryLake project 也能出统计(不再"requires local SQLite"报错)。
- `mem_doctor`:MemoryLake project 时重定义为连通/鉴权/延迟体检(或明确、可预期的降级),不再硬报错;SQLite project 不变。
- `addPromptIfMissing` seam:让 MemoryLake project 的自动 prompt 捕获走 `MemoryBackend.AddPromptIfMissing`(Task 13 已实现),不再 no-op。
- 测试:MemoryLake stub 后端下 mem_stats/prompt 捕获生效;SQLite 路径不变。

## Task 18: parity 完整矩阵 + 实跑

**Files:** `internal/paritytest/*`、`docs/.../parity-report-2026-07-22.md`

- 按 parity 规格补全 §4 矩阵(每接口多 case,EXACT/SEMANTIC/SET_RANK/BEHAVIOR/UNSUPPORTED),LLM-judge 用当前 provider。
- 用真实 `ENGRAM_MEMORYLAKE_API_KEY` 跑 `go test -tags parity`,临时 project 隔离、用后即删。
- 产出记分卡 `parity-report`,判定各接口 MemoryLake vs SQLite 是否满足门槛(#5),不达标项列出。
