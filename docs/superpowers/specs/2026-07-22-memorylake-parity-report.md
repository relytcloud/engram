# Parity 实测记分卡:MemoryLake vs SQLite

- 日期:2026-07-22
- 方式:对真实 MemoryLake 租户(`engram` workspace,`app.memorylake.ai`)实跑 `internal/paritytest`(`//go:build parity`)+ 直接 API 探测,与本地 SQLite 后端对比。
- 关联:`2026-07-22-memorylake-sqlite-parity-testing.md`(方法论)、`docs/superpowers/plans/2026-07-22-memorylake-backend-phase2.md`(Task 18)。

## 摘要结论

| 维度 | SQLite | MemoryLake | 谁更好 |
|---|---|---|---|
| **返回内容保真** | 逐字原文 | 逐字原文(经 `engram_raw`) | **平**(设计保证) |
| **语义索引文本** | 原文 | LLM 抽取改写("Decided that … (as of date)") | SQLite(但不影响返回内容) |
| **写后可读延迟** | 同步、即时 | 异步 **~12s–>90s(高度可变)** | **SQLite** |
| **关键词检索** | FTS5/BM25(title 权重 5) | 语义向量 + `fact_fuzzy` 子串兜底 | 场景相关(见下) |
| **语义/自然语言检索** | BM25(弱) | 向量语义(强) | **MemoryLake** |
| **多人共享 / 冲突** | 无(本地单机) | actor 分区 + workspace | **MemoryLake** |
| **合并项目 / 硬删 / 本地体检** | 支持 | 不支持 / 软删 / 重定义 | SQLite |

**一句话**:内容保真两者持平(靠 `engram_raw`);MemoryLake 的价值在**共享 + 语义检索**,代价是**显著且可变的写后延迟**和若干本地能力缺失。#5「准确性不低于 SQLite」在**返回内容**层面成立;在**检索命中**层面是"不同强项"而非单调更优。

## 逐维度实测

### 1. 内容保真(EXACT)— 平
- 实测:向 MemoryLake 存 `"Decision: Engram uses modernc.org/sqlite with CGO_ENABLED=0 for a pure-Go build."`,LLM 抽取出的 fact 是 `"Decided that Engram uses modernc.org/sqlite with CGO_ENABLED=0 for a pure-Go build (as of 2026-07-22)"`(**改写 + 加日期**)。
- 但 Engram 读回时取 `metadata.engram_raw` = **逐字原文**,故 `mem_get`/`mem_search` 返回的 `content` 与 SQLite 完全一致。
- **判定:平**。抽取改写只污染语义索引,不污染返回内容 —— 这正是 `engram_raw` 设计的目的。

### 2. 写后可读延迟 — SQLite 明显更好
- 实测抽取延迟**高度可变**:早期测试 ~12s;后续同类内容 **>90s** 才出现 fact。
- 影响:`mem_save` 的有界同步回填(默认 maxWait 30s,实测 90s 仍可能超时)常返回 **provisional id**;该 id 映射到 message(`conv-entry-…`)而非 fact,**无法用于随后 `mem_get`**(返回 404)。
- SQLite 同步即时,无此问题。
- **判定:SQLite 更好**。这是已接受的架构代价,但实测显示延迟比预期更大更不稳定,`mem_save` 后应视为"最终一致"。

### 3. 检索 — 场景相关
- MemoryLake 检索是**语义向量**(基于抽取后的 fact 文本)+ `fact_fuzzy` 子串兜底;SQLite 是 **BM25 关键词**(title 权重 5)。
- 语义/自然语言查询:MemoryLake 更强(向量召回)。
- 精确标识符(函数名/报错串):SQLite 的 BM25 更稳;MemoryLake 靠 `fact_fuzzy` 子串命中原文兜底。
- **完整 recall@k 量化**需 eventual-read harness(见下 §未竟),本轮因抽取延迟未量化;定性判定为"不同强项,非单调"。

### 4. 不支持 / 降级(UNSUPPORTED)— 契约正确
- `mem_merge_projects`:MemoryLake 返回明确 "does not support merging projects" ✓(parity `TestMergeProjects_Unsupported` PASS)。
- 硬删:降级为软删(forget)。`mem_doctor`:重定义为连通/鉴权体检。
- **判定:契约化降级正确,无编造行为**。

## 实跑发现并修复的真实 bug(mock 测试全部漏掉)

实跑对真实 API 是本阶段最高价值来源 —— 抓到 3 个只在真实端点暴露的 bug:

1. **`page_size=200` 被拒**:workspace/project/actor 列表端点上限 100(facts 才 200)。→ 修 `identity.go` 三处为 100(commit)。
2. **会话创建非幂等**:`ensureConversation` 假设重复 custom_id 幂等返回既有会话,真实 API 返回 **409 CUSTOM_ID_CONFLICT** → 同一 session 第 2 条 `mem_save` 起会失败。→ 修为 409 时按 `by_custom_id` GET 既有会话(commit `c0e0f15`)。
3. **actor 绑定非幂等**:重复绑定返回 **409 BINDING_ALREADY_EXISTS** → `NewBackend` 失败。→ 修为把该 409 当幂等成功(commit `c0e0f15`)。

## 未竟(harness follow-up,不阻塞功能)
- **eventual-read**:EXACT/SET_RANK 需在 `AddObservation` 后轮询到 fact 抽取完成(经 Search 找回真实 fact id)再比对,而非依赖有界等待 —— 因抽取可 >90s。据此可量化 recall@k/MRR。
- **临时项目自动清理**:`NewMemoryLakeBackend` 目前只 forget facts,不删临时 project(Client 未暴露 project-delete;V3 有 `DELETE …/projects/{id}`)。本轮遗留的 `engram-parity-*` 项目已手动清理;后续给 Client 加 `DeleteProject` 并挂 `t.Cleanup`。
- **provisional id 可解析性**:`mem_save` 超时返回的占位 id 当前不可 `mem_get`;可考虑改为返回 message 引用 + 状态查询。
