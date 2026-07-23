# 设计:Engram × MemoryLake 薄适配器(Option A)—— 能力下沉,删 idmap,id 改字符串

- 日期:2026-07-23
- 状态:待评审
- 前提:功能已在 PR #1(Phase 1+2)落地并可用。本规格是**下一阶段(Phase 3)重构**,只作用于 **MemoryLake 后端路径**;**未 enable 的 project(SQLite)行为除 id 类型放宽外不变**。
- 背景调研:`memorylake-backend`(Java)只是多租户/ACL/OpenAPI 门面;真正的记忆智能在下游 `powerdrill-knowledge` 的 **mem0/fact**(Python):抽取 → 向量+BM25 混合检索既有记忆 → **LLM 自动 ADD/UPDATE/FORGET/NOOP 决策** → 去重、矛盾即合并、冲突检测、temporal。**默认启用、是主路径。** Engram 现有的去重/upsert/冲突候选与之重复且会打架。

## 1. 决策(已与用户确认)

- **哲学**:Engram-on-MemoryLake 瘦身为**薄适配器**,把去重/更新/冲突/内容管理**下沉给 mem0**;Engram 只保留 mem0 没有的差异化能力。
- **A1**:**删除 idmap 及其全局唯一化机制**;工具的 observation `id` 放宽为**不透明字符串**(MemoryLake = `fact-id`;SQLite = 数字串)。
- **检索**:MemoryLake 路径**纯语义**(用 mem0 对外 search);**删除** `fact_fuzzy` 关键词兜底。
- **放弃逐字保真**:读回返回 mem0 抽取/合并后的 fact 文本;**删除** `engram_raw` 存储与读时优先。

## 2. MemoryLake 路径要**删除**的东西

| 删除项 | 原因(mem0 已原生) |
|---|---|
| hash 去重(normalized_hash + dedupe window) | mem0 Stage2 向量+BM25 去重 + update-prompt NOOP |
| topic_key upsert(PATCH-in-place)+ `topicindex.go` | mem0 LLM UPDATE 决策 + history 版本 |
| `FindCandidates` 的候选生成(FTS 风格) | mem0 冲突检测 m2m + 矛盾即合并 |
| `engram_raw` 回填 + 读时优先原文 | 接受 mem0 改写后的 fact 文本 |
| **同步等待抽取(bounded backfill)** | 不再需要回填 metadata → **mem_save 秒回** |
| **`idmap.go` + 全局唯一 int64 + `factForID` 守卫 + 读时登记** | id 直接用 fact-id 字符串,天然全局唯一 |
| `fact_fuzzy`(search.go 关键词兜底) | 纯语义 |

> 连带红利:删掉同步回填后,之前"存一条卡几十秒"的问题消失;删掉 idmap 后,跨项目 id 泄露、跨机句柄、全局唯一那一整套复杂度与其 bug 面全部消失。

## 3. `id` 类型放宽(A1 的核心机械改动)

把 **`MemoryBackend` 接口的 by-id 方法参数从 `int64` 改为 `string`**,统一为不透明 id:

```
GetObservation(id string) / UpdateObservation(id string, …) / DeleteObservation(id string, …)
PinObservation(id string) / UnpinObservation(id string) / Timeline(id string, …)
FindCandidates / JudgeBySemantic 里的 obs id 亦为 string
AddObservation(…) (string, error)   // 返回不透明 id(见 §4)
```

- **SQLite 后端**:内部把字符串 id `strconv.ParseInt` 回 int64 用(store 仍是 int64 PK);对外呈现为数字串。
- **MemoryLake 后端**:字符串 id **就是** fact-id,直接调 MemoryLake。
- **mcp handlers**:`id` 从 args 读为字符串(工具 schema `id` 由 number → string)。
- **HTTP server / TUI / CLI**:by-id 路径同步放宽为字符串。

**受影响的对外契约(全局,非仅 MemoryLake 项目)**:`id` 字段类型 number → string,含 **SQLite 默认路径**。
- Agent(Claude Code 等):透传 id,基本无感。
- **下游 plugin(OpenCode/Pi/obsidian)/ dashboard**:若假设 id 为数字需一并放宽为字符串。**用户已批准修改 plugin** —— 纳入本次范围(见 §9),不再是约束张力。
- 现有断言 int64 id 的测试(SQLite 侧亦然)需相应更新。

## 4. 各 mem_* 在薄适配器下的行为

| 工具 | 薄适配器行为 |
|---|---|
| **mem_save** | append 消息到 conversation → **立即返回轻量 ack**(不等抽取);facts 由 mem0 异步 ADD/UPDATE/NOOP 决定(N:M)。返回的 id 为 message/pending 引用(字符串);**不再保证"存完即可 by-id get"**(靠 search 找回真实 fact-id)。 |
| **mem_search** | 纯语义:`POST …/memories/search`(project/actor 过滤);返回 fact 的 `id`(fact-id 字符串)、score、内容(mem0 fact 文本)。 |
| **mem_get_observation(id)** | `GET …/facts/{fact-id}` 直取。 |
| **mem_update(id,…)** | `PATCH …/facts/{fact-id}`(用户显式改写;注意 mem0 后续仍可能再合并)。 |
| **mem_delete(id)** | `POST …/facts/{fact-id}/forget`(软过期)。 |
| **mem_pin/unpin(id)** | PATCH fact metadata `pinned`(Engram 侧展示用;mem0 不认)。 |
| **mem_timeline(id)** | fact list 按 created_at 排序取邻居(或映射 fact trace,若 OpenAPI 可达)。 |
| **mem_judge / mem_compare** | 改读 **conflict API(v2 list/get/resolve;v3 list 是 stub)**;能返回什么取决于该租户是否开启 mem0 冲突检测。Engram 不再自己生成候选。 |
| **mem_review** | **保留**(mem0 无 recency decay):客户端按 type 衰减 + fact expiration_date。 |
| **mem_stats / mem_context / sessions / prompts / passive** | 沿用现有映射(list/统计、conversation、ExtractLearnings)。 |
| **mem_merge_projects / 硬删 / mem_doctor** | 不支持 / 软删 / 重定义(不变)。 |

## 5. Engram 在 MemoryLake 路径**保留**的差异化能力
- **mem_review 时间衰减**(mem0 没有 recency decay / 按时间排序召回)。
- **pinned**(Engram 展示层,存 fact metadata)。
- **sessions/prompts/passive** 的 Engram 语义包装。
- SQLite 后端(默认)**完整保留**所有原生能力,一字不改(除 id 呈现为字符串)。

## 5.5 MemoryLake 路径:最小工具面 + 精简 agent 协议

**约束前提**:MCP 工具是 SQLite 与 MemoryLake **两个后端共用、全局注册一次**的;默认 SQLite 后端仍需要 update/delete/judge 等显式管理。所以**不删工具**,只做两件事:(a) MemoryLake 路径上把 mem0 已代劳的工具**变薄/退化**;(b) **精简 agent 协议**引导,让 agent 对 MemoryLake 项目回归"save + search + context"核心。

### (a) MemoryLake 路径工具分层

| 层 | 工具 | MemoryLake 路径行为 |
|---|---|---|
| **核心(agent 主要用)** | `mem_save` `mem_search` `mem_context` | save 喂对话→mem0 抽取/去重/合并;search 语义召回;context 拉最近+pinned |
| **显式控制(仍需要)** | `mem_delete`(forget)`mem_get_observation` | mem0 的 FORGET 只对显式遗忘触发,故保留;get 按 fact-id 直取 |
| **Engram 增量(mem0 没有)** | `mem_review`(衰减)`mem_pin/unpin` `mem_session_*` `mem_save_prompt`/`passive` | 保留;prompt/session = 对话消息(见 §关于 prompts) |
| **退化/交给 mem0** | `mem_update` `mem_judge` `mem_compare` `mem_suggest_topic_key` | **mem_update**:仍允许显式 PATCH,但正确姿势是"再 save 一次让 mem0 合并";**mem_judge/mem_compare**:Engram 不再本地生成候选 → 这条 loop 自然沉默;可选择改读 conflict API 暴露 mem0 已检出的冲突,或直接不引导 agent 用;**suggest_topic_key**:topic_key 语义弱化(mem0 自管合并),保留纯函数但不强调 |
| **不支持** | `mem_merge_projects` 硬删 `mem_doctor`(重定义) | 不变 |

### (b) agent 协议精简(memory protocol)

现协议(SessionStart 注入,全局)引导 agent:save 后若 `judgment_required` 就走 `mem_judge` 冲突循环、并强调主动 dedup/update。在 MemoryLake 下:

- **冲突判定循环天然沉默**:MemoryLake 路径 `mem_save` **不再返回 candidates/judgment_required**(本地候选生成已删)→ agent 永远不会进入 `mem_judge`/`mem_compare` 环节。**这是"精简"的主要来源 —— 靠行为自然达成,不需逐项目发不同协议。**
- **协议文案调整**(plugin 可改,已批准):核心仍是"主动 `mem_save` / 用 `mem_search` 回忆 / 开场 `mem_context`"(对两后端都对);**新增一句**:"若该 project 使用 MemoryLake 后端,去重/更新/矛盾合并由后端自动完成 —— 你只需 save 和 search,**不必手动 update/judge/compare**。" 
- **协议是会话级、非逐项目**(一个会话可能跨 SQLite/MemoryLake 项目),故采取:①核心引导普适;②冲突循环在 MemoryLake 上因无 candidates 自然不触发;③加上述一句条件说明。不追求"给 MemoryLake 项目单独一套协议"。

**净效果**:MemoryLake 项目上,agent 的实际用法收敛为 **save / search / context (+ 必要时 delete)**,其余交给 mem0 —— 达成用户要的"最小工具面",且不破坏 SQLite。

## 6. 给 MemoryLake 团队的接口需求(并行推进,不阻塞本重构)
1. **对外 search 完成 BM25 融合**(当前 `search_memories` 有 TODO,只返回向量结果)—— 补上后关键词场景不必 Engram 兜底。
2. **V3 conflict list 修复**(`MemoryConflictV3ServiceImpl.listConflicts` 现 `return null`)—— 否则 mem_judge/compare 只能走 v2。
3.(可选)**V3 暴露 `infer=false`** —— 若未来又想要逐字直存路径时用;本规格选择放弃逐字,暂不依赖。
4. 确认**冲突检测在目标租户已启用**(`conflict_detection_enabled`,代码默认 False、生产 helm 置 true)。

## 7. 数据/迁移
- 删除本地文件:`~/.engram/memorylake-idmap.json`、`memorylake-topics-*.json`(不再使用;可留旧文件不管,新代码不读)。
- 已 enable 的 project 无需迁移;既有 fact 保留,读回改为返回 fact 文本。

## 8. 风险 / 待确认
- **id number→string 全局契约变更(已接受)**:含 SQLite 默认路径;plugin/dashboard 一并放宽(用户已批准改 plugin)。实现前先**核实哪些 plugin/dashboard/HTTP 端点真的按数字解析 id**,逐一放宽 + 加测试。
- **mem_save 返回 id 的可用性**:异步下返回的是 pending 引用,不能立即 by-id get —— 与 SQLite 的"存完即得 id"语义不同,需在工具描述里说明"新存内容经 search 找回"。
- **mem_judge/compare 依赖租户开了冲突检测**:否则空;需实测目标租户状态。
- **测试**:parity/差分需相应更新(不再比逐字保真,改比"语义召回 + 行为等价");SQLite 路径逻辑回归必须全绿(仅 id 呈现由数字变字符串)。

## 9. 范围小结
本重构(Phase 3)=
1. **删** `internal/memorylake` 的 `idmap.go`/`topicindex.go`/hash 去重/backfill 同步等待/`engram_raw`/`fact_fuzzy`;
2. **改** `MemoryBackend` 接口 by-id 方法 `id` 由 `int64` → `string`;SQLite 后端内部 `ParseInt` 适配(store 仍 int64 PK);
3. **重定向** `mem_judge`/`mem_compare` → conflict API(v2);`FindCandidates` 不再本地生成候选;
4. **放宽为字符串**:MCP 工具 `id` schema、`internal/server` HTTP by-id 端点、`internal/tui`、`cmd/engram` CLI by-id;
5. **同步更新** `plugin/*`(pi/opencode/obsidian)与 `internal/cloud/dashboard` 中任何按数字解析 observation id 的地方;
6. **mem_save** 去掉同步回填 → append 即返回(秒回);
7. **保留** `mem_review` 衰减、pinned、sessions/prompts/passive;SQLite 默认路径**逻辑**不变(仅 id 呈现为字符串)。
8. 更新 parity/差分测试(不再比逐字,改比语义召回+行为等价)。
9. **最小工具面 + 精简 agent 协议**(§5.5):MemoryLake 路径 `mem_save` 不再返回冲突候选 → judge/compare 循环自然沉默;调整 memory-protocol 文案(plugin 可改),引导 MemoryLake 项目只用 save/search/context(+delete),其余交 mem0。工具集不删(SQLite 仍用),仅行为变薄 + 协议引导。

**分阶段安全落地建议**:先做 (2)(4)(5) 的 id→string 契约统一(SQLite 全绿、plugin 更新),再做 (1)(3)(6) 的 MemoryLake 能力下沉 —— 两步各自可独立验证回归。
