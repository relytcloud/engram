# 设计:Engram 接入 MemoryLake API 作为逐 project 可选后端(V3 facts)

- 日期:2026-07-22
- 状态:待评审(brainstorming 产出的设计规格)
- 范围:**只改 `engram` 服务本身**,不改任何 plugin / agent / MCP 工具对外契约(工具名、入参名保持不变)。
- 目标读者:实现该迁移的工程师。

---

## 1. 背景与决策记录

### 1.1 目标(用户提出)
1. 准确理解并正确使用 MemoryLake API。
2. 把 Engram 后端**全部换成 MemoryLake API**,所有 `mem_*` 工具都走 memory API;不能映射的给出方案。
3. 接口上可配置 MemoryLake 的 API 地址与 API key。
4. 记忆存储在 **zbyte 组织 → `engram` workspace** 下。
5. 整体效果/准确性不低于原 SQLite 方案。
6. 保留现有 Engram 能力。
7. 多人共享同一 project 时避免 conflict。
8. 产出可落地实现的文档。

### 1.2 关键实测发现(决定架构)
所有结论均通过对 `https://app.memorylake.ai/openapi/memorylake` 的真实 API 调用验证:

| 发现 | 证据 | 影响 |
|---|---|---|
| `engram` workspace 真实存在 | `GET /api/v3/workspaces` → `ws-f8d7299925214dc489b11e9fd5dc50e2`(custom_id 可用 `engram`) | 落点确定 |
| **V2 与 V3 是两套独立命名空间** | V3 建的 project 用 V2 记忆 API 访问 → `Project not found` | 只能二选一 |
| V2 记忆 API 不认 workspace | `/api/v2/projects/{id}` 是租户顶层 | V2 不满足 #4 |
| V2 `Memory` 读模型**不返回 metadata** | `common/client/dto/Memory.java` 仅 id/content/user_id/expired/时间戳 | V2 无法还原 Engram 字段 |
| **V3 fact 写入永远经 LLM 抽取,无 `infer:false`** | 全仓无 `infer`;`ConversationCreateRequest`/`MessageCreateRequest` 无该字段 | V3 无法逐字保真 |
| 1 条 observation → **被改写并拆成 N 条 fact** | 实测:1 条消息 → 2 条 fact,措辞被改、加了 "(as of date)" | 破坏 1:1 记录模型 |
| 写时消息 metadata **不继承**到 fact | 实测 fact metadata 只有自动 `keywords` | 需抽取后 PATCH 回填 |
| **PATCH fact metadata 完整 round-trip** | 实测回填 `{engram_type,scope,topic_key,tags,pinned,engram_obs_id}`,GET/search 均原样返回 | ✅ 回填方案可行 |
| 语义搜索返回 `score` + metadata | 实测 `score:0.498` + 我 PATCH 的 metadata | ✅ 可重建 Engram 响应 |
| 抽取延迟 ~12s | 消息 04:52:50 → fact 04:53:02 | 需异步 read-after-write 处理 |
| V3 fact 有 update/forget/trace/conflicts | 各 controller 已确认 | 大部分工具可映射 |

### 1.3 决策:按 project 选择后端,MemoryLake 逐 project opt-in(V3-facts)
在**已知上述权衡**的前提下,用户明确选择:

- **默认不启用 MemoryLake**。`internal/store`(SQLite)**保留**为默认后端;只有用户对某个 project **显式 enable** 后,该 project 的 `mem_*` 才路由到 MemoryLake。未 enable 的 project 行为完全不变(逐字保真、FTS5/BM25、1:1 int id 全保留)。
- MemoryLake 后端的存储模型:**V3 facts,挂 `engram` workspace 下**(满足 #4、多人 conflict)。
- 对 **已 enable 的 project**,**接受抽取语义,重定义"准确"**:"准确" = MemoryLake 语义召回质量,而非逐字保真。

**已 enable project 上被明确接受的代价(仅作用于这些 project):**
- 记忆内容会被 LLM 改写/拆分,非逐字原文。
- 一条 `mem_save` 可能产生**多条** fact,对外"一条记忆"不再严格 1:1。
- `mem_save` 从同步返回改为**异步**(先返回 pending)。
- `pinned` 从"仅本地设备状态"变为**共享**状态(存 fact metadata)。
- 对外 id 从自增 int 变为 fact-id 字符串(见 §4.3)。
- `mem_doctor` 语义重定义;`mem_merge_projects` 在 MemoryLake project 上**不支持**(见 §6)。

> 这一"逐 project opt-in"决策同时消解了 §1.2 的核心张力:需要逐字保真的 project 留在 SQLite,需要多人共享/云端语义的 project 才切 MemoryLake。#5(准确不降)与 #6(能力全保留)对**未 enable 的 project 天然成立**;对已 enable 的 project 按上述重定义执行。

---

## 2. 架构总览

```
agent → cmd/engram (CLI + MCP dispatch, 契约不变)
          → internal/mcp (mem_* handlers, 契约不变)
              → MemoryBackend 接口(按 project 选择,§3.3)
                  ├─ 未 enable(默认)→ internal/store (SQLite, 不变)
                  └─ 已 enable       → internal/memorylake (新增:V3 client + 映射层)
                                          → HTTPS → app.memorylake.ai/openapi/memorylake (V3 API)
```

- **保留**:`internal/store`(SQLite/FTS5/relations)作为**默认后端**,一字不改地服务所有未 enable 的 project。`internal/sync`、`internal/cloud/autosync` 仅对 SQLite project 有意义,保留。
- **抽象**:定义 `MemoryBackend` 接口(方法签名与现有 `store` 对 mem_* 的调用等价)。`SQLiteBackend` = 现有 store 封装;`MemoryLakeBackend` = 新实现。handler 依 §3.3 逐 project 选择实现。
- **新增**:`internal/memorylake` 包,承担:
  - `client.go` — V3 REST client(鉴权、重试、错误映射、分页/cursor)。
  - `identity.go` — workspace / project / actor 解析与自动 provision(带本地缓存)。
  - `mapper.go` — Engram Observation ⇄ V3 fact + metadata 编解码。
  - `writequeue.go` — 复用现有 `internal/mcp/write_queue.go` 思路,承载异步"append → 轮询抽取 → PATCH 回填"。
  - `search.go` — 语义搜索 + `fact_fuzzy` 子串兜底 + 客户端 type/scope/pinned 过滤与排序。
- **不动**:`internal/mcp` 的工具定义(`mcp.NewTool(...)`)与入参 schema;`internal/project`(cwd 项目探测);`internal/tui`/`internal/server` 对 store 的调用改为走 `internal/memorylake`(接口保持等价方法签名,最小改动)。

### 2.1 设计原则
- `internal/mcp` 的 handler **不感知**具体后端;通过 `MemoryBackend` 接口交互,按 project 注入 SQLite 或 MemoryLake 实现,便于测试(mock)。
- 所有对外 tool 响应字段名保持不变;语义变化(异步、N facts)通过**新增**字段(如 `status:"pending"`)表达,不删旧字段。
- 两种后端可**并存**:同一台机器上,project A 走 SQLite、project B 走 MemoryLake;`id` 参数放宽为字符串,SQLite project 内仍是数字串、MemoryLake project 是 `fact-...`,由 project 后端决定解析方式。

---

## 3. 身份与配置

### 3.1 三层身份映射

| Engram 概念 | MemoryLake V3 概念 | 解析/创建方式 |
|---|---|---|
| 组织(隐式) | Tenant(API key 绑定) | 无需指定,key 决定 |
| 固定命名空间 | **Workspace `engram`** | 配置 `ws-...` 或用 custom_id `engram` 解析(`GET /api/v3/workspaces` 匹配 name/custom_id) |
| Engram project(cwd 探测) | **Project**(挂 engram workspace) | `custom_id = NormalizeProject(项目名)`;首次 `POST /api/v3/workspaces/{ws}/projects` 自动建,结果缓存 |
| 记忆归属(本机/人/agent) | **Actor**(HUMAN/ASSISTANT) | `custom_id = 机器或用户标识`;`POST /api/v3/actors` + 绑定 `POST /api/v3/workspaces/{ws}/actors`;或用 TenantProvision 幂等 provision |

- **project ⇄ V3 project custom_id**:用 `NormalizeProject`(小写+trim,与旧逻辑一致)保证同名幂等。缓存 `projectName → proj-id` 于本地配置文件(`~/.engram/memorylake-cache.json`),避免每次调用都 list。
- **多人共享**:同一 project 下每个人是一个 bound actor;写入时 `actor_id` 区分来源;搜索默认按当前 principal 的 actor,可显式跨 actor(见 §7)。

### 3.2 配置项(满足接口要求 #3)
新增环境变量(或 `~/.engram/config` 段):

```
ENGRAM_MEMORYLAKE_BASE_URL   = https://app.memorylake.ai/openapi/memorylake   # 必填(启用时)
ENGRAM_MEMORYLAKE_API_KEY    = sk-...                                          # 必填(启用时)
ENGRAM_MEMORYLAKE_WORKSPACE  = engram         # workspace custom_id 或 ws-id,默认 "engram"
ENGRAM_MEMORYLAKE_ACTOR      = <machine-id>   # 可选,默认取 hostname/用户;缺省自动 provision
ENGRAM_MEMORYLAKE_TIMEOUT_MS = 30000          # 可选
ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS = 2000      # 抽取轮询间隔
ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS = 30000 # 抽取轮询上限
```

- 鉴权:仅 `Authorization: Bearer <key>`。**不需要也不接受** workspace/tenant header(网关会剥离伪造头);workspace/project 只走 URL path。

### 3.3 逐 project 后端选择(核心:默认不启用 MemoryLake)

**默认关闭。** 一个 project 只有被**显式 enable** 后才路由到 MemoryLake;否则一律走 SQLite(`internal/store`,行为不变)。

- 启用清单持久化在本地 `~/.engram/memorylake.json`:
  ```json
  { "enabled_projects": { "acme-api": { "proj_id": "proj-...", "enabled_at": "..." } } }
  ```
- 新增 CLI:
  ```
  engram memorylake enable  --project <name>   # 首次会自动在 engram workspace 建 project 并写入清单
  engram memorylake disable --project <name>   # 移出清单,回退 SQLite
  engram memorylake status                     # 列出各 project 当前后端
  ```
- **后端选择(每次 mem_* 调用)**:handler 先解析 project(cwd 或入参)→ 查启用清单:
  - 命中 → `MemoryLakeBackend`;
  - 未命中 → `SQLiteBackend`(默认)。
- `ENGRAM_BACKEND=sqlite|memorylake`(§9)是**全局回退开关**,用于整机对比/回滚;不设或设 `sqlite` 时,即使 project 在清单里也强制走 SQLite(安全阀)。正常运行下由**逐 project 清单**决定。
- 全局默认(无清单、无环境覆盖)= **SQLite**。启用某 project 时对该 project 做一次连通性校验(见 §6 `mem_doctor`)。

---

## 4. 数据模型映射

### 4.1 Observation → V3 fact

一条 Engram observation 在 MemoryLake 中体现为**一条或多条 fact**(抽取决定)。Engram 侧字段编码进 **fact metadata**(抽取后 PATCH 回填):

| Engram 字段 | 落点 | 说明 |
|---|---|---|
| `content` | fact.`fact`(抽取后文本,仅作语义索引) + **metadata.`engram_raw`(逐字原文)** | 见 §4.2:读回默认返回 `engram_raw` 原文,保住 #5 保真 |
| `title` | metadata.`engram_title` | |
| `type` | metadata.`engram_type` | decision/bugfix/... |
| `scope` | metadata.`engram_scope` | project/personal |
| `topic_key` | metadata.`topic_key` | 用于 upsert 语义模拟(见 §5.3) |
| `tags` | metadata.`tags`(数组) | |
| `pinned` | metadata.`pinned`(bool) | 共享语义 |
| `session_id` | metadata.`engram_session` = conversation custom_id | |
| `sync_id`/对外 id | **fact id(`fact-...`)** | 取代自增 int id;见 §4.2 |
| `revision_count` | metadata.`engram_rev` | PATCH 时自增 |
| `created_at`/`updated_at` | fact.`created_at`/`updated_at` | 服务端维护 |
| `deleted` | fact `forget`(软删=expired) | |
| `review_after` | fact.`expiration_date`(近似)或 metadata.`engram_review_after` | 见 §6 mem_review |

metadata 是 `Map<String,Object>`,读/搜/list 均返回(已实测)。

### 4.2 逐字原文保真(缓解抽取改写)

因 V3 fact 必经 LLM 抽取(改写+拆分),**在记忆内容里保存原始文本**以保住保真:

- **写入**:PATCH 回填时始终写 `metadata.engram_raw = <观测原文逐字>`(单条 observation 的完整原文;拆成 N 条 fact 时,每条 fact 的 `engram_raw` 都指向同一份原文,并用 `engram_obs_id` 归组)。
- **读回(mem_get_observation / mem_search / mem_context)**:Engram 重建 Observation 时,`content` **优先取 `engram_raw`(逐字原文)**,`fact.fact`(抽取文本)仅用于:① 语义搜索的召回索引,② 无 `engram_raw` 时(如控制台直接产生、非 Engram 写入的 fact)的回退显示。
- **可选**:同时把原文以 `\n\n---原文---\n<raw>` 形式 append 进 `fact.fact`,让 MemoryLake 控制台与 `fact_fuzzy` 子串搜也能命中逐字内容(代价:fact 文本变长、语义向量略被稀释;默认关闭,置于配置 `ENGRAM_MEMORYLAKE_EMBED_RAW_IN_FACT=false`)。

> 效果:即便语义索引是抽取后的改写文本,**agent 拿到的仍是逐字原文**,把 #5 的保真损失压到最小;"重定义为语义召回"仅体现在检索命中率层面,不体现在返回内容上。

### 4.3 对外 id 模型(重要变更)
- 旧:自增 int `id`。新:**fact id 字符串 `fact-...`**。
- `mem_*` 工具入参里凡是 `id`/`observation_id`/`memory_id_a`(旧为 int),改为**接受字符串 fact id**;handler 内部不再做 int 解析。这是对外可见的**类型放宽**(int→string),需在工具描述里注明,但不改参数名。
- 一条 `mem_save` 产生多条 fact 时,响应返回 `fact_ids: []`;后续 update/delete/get 针对单条 fact id。

---

## 5. 写路径(mem_save 及派生)

### 5.1 流程(异步,已获用户确认选 (a))
```
mem_save(title, content, type, scope, topic_key, project, session_id, ...)
  1. 解析 project → proj-id(缓存/自动建)
  2. 解析/复用 conversation:以 session_id 为 conversation.custom_id
     - 不存在则 POST /api/v3/workspaces/{ws}/memories/conversations
       { custom_id: session_id, kind: "DIRECT", actor_ids:[actor], rw_project_ids:[proj-id] }
  3. append message:
     POST /api/v3/conversations/{convId}/messages
       { custom_id: <obs 稳定 hash>, actor_id, content:[{block_type:"TEXT", text: content}] }
  4. 立即返回 { conversation_id, message_id, status:"pending", note:"facts 抽取中" }
  5. 后台(write queue worker):
     a. 轮询 GET .../projects/{proj}/memories/facts(或按 conversation 过滤)取新 fact
        —— 轮询间隔 EXTRACT_POLL_MS,上限 EXTRACT_MAX_WAIT_MS
     b. 对每条新 fact PATCH metadata 回填 §4.1 字段(含 engram_raw 原文、topic_key、engram_msg=message custom_id 便于关联)
     c. 记录 message custom_id → fact_ids 映射(本地缓存,供后续引用)
```

- **幂等**:message `custom_id` 用 observation 归一 hash;重复 `mem_save` 相同内容 → 同 message,不重复抽取(MemoryLake 对重复 custom_id 返回既有 message)。
- **返回契约变化**:`mem_save` 不再同步返回 `id`/`judgment_required`/`candidates`。新增 `status:"pending"` 与 `message_id`。冲突候选由 MemoryLake 异步 conflict 检测产生(见 §5.4),`mem_judge` 改为查 conflict 列表。

### 5.2 mem_capture_passive / mem_session_summary
- 走同一写路径:解析出的每条 learning append 一条消息;summary 作为一条消息(type=`session_summary`)。
- dedupe:message custom_id = 内容 hash,天然幂等,替代 SQLite 的 normalized_hash dedupe。

### 5.3 topic_key upsert 模拟
MemoryLake 无原生 upsert。实现:
1. 写前 `GET .../facts?fact_fuzzy=<topic_key>` 或按缓存查 `topic_key → fact_ids`;
2. 命中(同 project+scope+topic_key)→ 对既有 fact `PATCH { fact: 新内容, metadata: {...engram_rev+1} }`,**不再 append 新消息**;
3. 未命中 → 走正常 append 抽取路径。

> 注意:因抽取是 1→N,upsert 只能作用于"已回填 topic_key 的 fact";首次写入的 topic_key 关联在 §5.1 步骤 b 建立。文档标注此为**近似 upsert**,以 topic_key 命中缓存为准。

### 5.4 多人 conflict(#7)
- 多人共享 project → 各自 actor 写入 → MemoryLake **内建 conflict 检测**自动发现矛盾/重复。
- Engram 不再自己用 FTS 生成冲突候选;`mem_save` 后如需提示,查 `GET /api/v3/workspaces/{ws}/memories/conflicts`(或 project 级)。
- 写并发:同一 conversation 的消息必须顺序 append(并发 409);Engram write queue 已串行化写,天然规避;跨 conversation/actor 无此限制。

---

## 6. 逐个 mem_* → MemoryLake V3 映射表

> 所有路径外部前缀 `/openapi/memorylake`;`{ws}` = engram workspace,`{proj}` = 解析出的 project,`{actor}` = 当前 actor。

| 工具 | 映射到 V3 | 契约变化 / 备注 |
|---|---|---|
| **mem_save** | `POST .../conversations` + `POST /api/v3/conversations/{c}/messages` + 后台 `PATCH .../facts/{id}` | 异步;返回 pending;1→N facts;见 §5 |
| **mem_search** | `POST /api/v3/workspaces/{ws}/memories/search` `{query, project_ids, actor_ids, memory_types:["fact"], top_k, threshold}` | 语义 + score;type/scope/all_projects/match_mode 在**客户端**按返回 metadata 过滤;关键词场景补 `GET .../facts?fact_fuzzy=` |
| **mem_get_observation** | `GET /api/v3/workspaces/{ws}/projects/{proj}/memories/facts/{factId}` | 返回 fact + metadata,重建 Observation JSON |
| **mem_update** | `PATCH .../facts/{factId}` `{fact?, metadata?}` | 只改传入字段;metadata 为整体替换,需先读后合并 |
| **mem_delete** | `POST .../facts/{factId}/forget`(软删) | `hard_delete=true` **无对应**(无硬删)→ 返回软删并在响应注明;或批量 `POST .../facts/forget` |
| **mem_context** | `GET .../facts`(按 actor+project list)+ 客户端 pinned 优先排序 + `GET .../conversations`(近期 session) | pinned 由 metadata 客户端过滤(list 无 metadata 服务端过滤) |
| **mem_pin / mem_unpin** | `PATCH .../facts/{factId}` `{metadata:{...,pinned:true/false}}` | 语义变共享;需先读 metadata 合并 |
| **mem_session_start** | `POST .../conversations` `{custom_id:sessionId, kind:DIRECT, actor_ids, rw_project_ids}` | session ↔ conversation |
| **mem_session_end** | `PATCH`/记录到 conversation metadata,或 append 一条 summary 消息 | conversation 无显式 end;用 metadata 标记 |
| **mem_session_summary** | 写路径(append summary 消息) | 见 §5.2 |
| **mem_save_prompt** | append 一条 role 隐含 user 的消息(或存 conversation metadata) | prompt dedupe 用 message custom_id |
| **mem_timeline** | `GET /api/v3/conversations/{c}/messages`(sequence_no 序)或 `GET .../facts` 按 `created_at` 排序取锚点前后 N | 无自增 id;用时间戳/sequence 重定义 |
| **mem_review** | `list` + 客户端按 `type` 衰减策略比对 `created_at`;或用 fact `expiration_date` | review_after 无服务端语义;客户端计算 |
| **mem_stats** | `GET /api/v3/workspaces/{ws}/statistics` + `GET .../projects/{proj}/statistics` + list 分页 total | 计数来自 statistics/分页 |
| **mem_judge** | `GET /api/v3/workspaces/{ws}/memories/conflicts/{id}` + `POST .../conflicts/{id}/resolve` | judgment_id → conflict id;6 verb → resolve strategy 映射(见 §6.1) |
| **mem_compare** | `POST .../conflicts/{id}/resolve` 或写入一条关系 fact | 语义化判定 → conflict resolve |
| **mem_suggest_topic_key** | **本地纯函数,不变** | 无网络 |
| **mem_current_project** | **本地 cwd 探测,不变**;`ProjectExists` 改为查 project 缓存/list | NEVER errors 语义保留 |
| **mem_doctor** | **重定义**:`GET /api/v3/workspaces/{ws}`(连通)+ 鉴权校验 + 延迟测量 + 抽取积压检查 | 不再体检 SQLite;检查项重写 |
| **mem_merge_projects** | **MemoryLake project 上不支持**(用户已确认不实现)。命中已 enable 的 project 时返回明确错误"MemoryLake 后端不支持项目合并"。SQLite project 行为不变 | 不实现云端版本 |

### 6.1 mem_judge 的 6-verb → conflict resolve 映射(建议)
Engram 的 relation verbs(related/compatible/scoped/conflicts_with/supersedes/not_conflict)与 MemoryLake `MemoryConflictResolveStrategy` 不完全同构。建议:
- `supersedes` → resolve strategy `keep_memory`(keep_memory_id = 新 fact)。
- `conflicts_with` → 保留冲突未决或按用户选择 resolve。
- `not_conflict` → resolve 为"非冲突"/忽略。
- `related`/`compatible`/`scoped` → MemoryLake 无对应,存为一条关系型 fact 的 metadata(`engram_relation`),或仅记录在客户端。
> 此映射为**近似**,文档标注:Engram 的关系图语义比 MemoryLake conflict 模型更细,部分 verb 仅客户端记录。

---

## 7. 读路径与"准确性"策略(#5 重定义后)

- **主检索**:V3 语义搜索,返回 score + metadata;Engram 用 metadata 做 type/scope/project 客户端过滤,用 score 排序,重建 `SearchResult{Observation, rank}`(rank = score)。返回的 `Observation.content` 取 `metadata.engram_raw`(逐字原文,见 §4.2),非抽取文本。
- **关键词兜底**:`GET .../facts?fact_fuzzy=<term>` 子串精确匹配,弥补语义搜索对标识符/报错串的弱项。`mem_search` 内部策略:query 含代码样 token(含 `/`、`_`、驼峰、`.`)时并行发起 fuzzy + semantic,合并去重。
- **topic_key 直查**:query 含 `/` → 先 `fact_fuzzy=topic_key` 命中置顶(对齐旧行为)。
- **多 actor 检索**:默认当前 actor;`all_projects`/跨人检索时传 `actor_ids=[]`(默认 principal)或显式多 actor,用 `GET /api/v3/workspaces/{ws}/memories/facts?actor_ids=...&project_ids=...`。

---

## 8. 错误处理、重试、一致性

- **鉴权/网络**:client 统一把 `ResponseWrapper{success:false, error_code}` 映射为 Go error;401/403 → 明确提示 key/权限;429 → 指数退避重试。
- **异步一致性**:mem_save 后 ~12s 内 fact 不可见。策略:
  - 写立即返回 pending;
  - 后台回填完成前,`mem_search`/`mem_get` 可能查不到刚写内容 —— 在工具描述里说明"新写入需数秒可检索";
  - 提供 message custom_id → fact_ids 本地缓存,`mem_save` 的调用方若需要可稍后凭 message_id 查询状态(可选新增 `mem_save` 的 `wait` 参数走阻塞模式 (b) 作为逃生舱)。
- **抽取产出 0 条 fact**:短内容可能不产生 fact;回填 worker 需处理"轮询超时仍 0 条",标记该 message 为"未抽取",避免无限轮询。
- **幂等**:所有创建走 custom_id;重试安全。

---

## 9. 迁移与开关(逐 project,不删 SQLite)

- **SQLite 保留**:默认后端不变,`internal/store`、`internal/sync`、`internal/cloud/autosync` 全部保留。本方案是**增量新增 MemoryLake 后端 + 逐 project 路由**,不是替换/下线本地栈。
- **enable 时的数据迁移(可选、按 project)**:`engram memorylake enable --project X [--migrate]` 时,若带 `--migrate`,把该 project 现有 SQLite observations → append 消息 → 回填 metadata(`engram_raw` 存逐字原文、原 `created_at` 写 message timestamp)。迁移**有损**(抽取改写,但读回用 `engram_raw` 原文);原 SQLite 数据**原样保留**,disable 即回退。
- **全局回退开关**:`ENGRAM_BACKEND=sqlite|memorylake`。缺省/`sqlite` = 安全阀,强制所有 project 走 SQLite(即使在启用清单里);`memorylake` = 遵循逐 project 清单。全局默认无需设置即为 SQLite。
- **可逆**:disable 一个 project 立即回退 SQLite;MemoryLake 侧数据保留在 engram workspace(可选一并 forget)。

---

## 10. 测试策略

- **client 单测**:用 httptest mock MemoryLake 各端点响应(含 ResponseWrapper 包装、分页 cursor、错误码)。
- **mapper 单测**:Observation ⇄ fact metadata 编解码往返。
- **契约测试**:每个 mem_* handler 用 mock `MemoryBackend`,断言对外响应字段不变(工具名/入参 schema 快照)。
- **e2e(可选、打 tag)**:针对真实 `engram` workspace 的 selftest project 跑写→轮询→读→搜→forget 全链路(用后即删),验证抽取延迟与 metadata round-trip。
- **准确性回归**:构造一组 Engram 典型查询(关键词 + 语义),对比 semantic+fuzzy 合并策略与旧 BM25 的召回,作为 #5"不低于"的度量基线。

---

## 11. 待验证/风险(实现期 spike)

1. **抽取延迟与稳定性**:~12s 实测于单条;批量/高峰下的分布未知 → 影响 write queue 超时参数。
2. **1→N 抽取的可控性**:能否通过消息措辞(如"记录如下事实:")降低拆分?影响 upsert 与 id 稳定性。
3. **conflict 检测触发时机与延迟**:mem_judge 依赖它,需实测 conflict 何时可见。
4. **statistics 端点字段**:mem_stats 计数精度未逐字段验证。
5. **actor 与 principal 关系**:实测 append 时返回的 actor_id 被服务端改写为 principal 自身 actor,多人分区的确切行为需专门验证(§3.1、§7)。
6. **fact_fuzzy 的匹配质量**:是否足以替代 BM25 的关键词精度,需回归度量(§10)。
7. `mem_merge_projects` 是否要真实现重 ingest,还是明确标注不支持。

---

## 12. 附:关键 V3 端点速查

```
# 身份
GET    /api/v3/workspaces                         # 解析 engram workspace
POST   /api/v3/workspaces/{ws}/projects           # 建 project(custom_id, name)
GET    /api/v3/workspaces/{ws}/projects
POST   /api/v3/actors                             # 建 actor(custom_id, actor_type, display_name)
POST   /api/v3/workspaces/{ws}/actors             # 绑定 actor({actor_id})
# 写
POST   /api/v3/workspaces/{ws}/memories/conversations   # {custom_id,kind,actor_ids,rw_project_ids}
POST   /api/v3/conversations/{conv}/messages            # {custom_id,actor_id,content:[{block_type:TEXT,text}]}
# fact 读/改/删
GET    /api/v3/workspaces/{ws}/projects/{proj}/memories/facts?fact_fuzzy=&page_size=&continuation_token=
GET    /api/v3/workspaces/{ws}/projects/{proj}/memories/facts/{factId}
PATCH  /api/v3/workspaces/{ws}/projects/{proj}/memories/facts/{factId}   # {fact?, metadata?}
POST   /api/v3/workspaces/{ws}/projects/{proj}/memories/facts/{factId}/forget
POST   /api/v3/workspaces/{ws}/projects/{proj}/memories/facts/forget     # {ids:[...]}
GET    /api/v3/workspaces/{ws}/memories/facts?project_ids=&actor_ids=&fact_fuzzy=   # 跨 project/actor
GET    /api/v3/workspaces/{ws}/actors/{actor}/facts
# 搜索 / 冲突 / 统计
POST   /api/v3/workspaces/{ws}/memories/search    # {query,project_ids,actor_ids,memory_types,top_k,threshold}
GET    /api/v3/workspaces/{ws}/memories/conflicts
POST   /api/v3/workspaces/{ws}/memories/conflicts/{id}/resolve
GET    /api/v3/workspaces/{ws}/statistics
GET    /api/v3/workspaces/{ws}/projects/{proj}/statistics
```

响应统一包装:`{ success, message, data, error_code }`。分页两种:V2 页码式 `{items,total,page,size,total_pages}`;V3 多为 cursor 式 `{items, continuation_token}`。
