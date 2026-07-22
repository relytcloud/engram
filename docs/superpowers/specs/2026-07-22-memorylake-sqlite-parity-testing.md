# 测试规格:SQLite vs MemoryLake 双后端差分对比

- 日期:2026-07-22
- 状态:待评审
- 关联:`2026-07-22-memorylake-backend-design.md`(设计)、`docs/superpowers/plans/2026-07-22-memorylake-backend.md`(实现计划)
- 目的:为 **#5「准确性/效果不低于 SQLite」** 提供可执行的度量。对 `MemoryBackend` 的每个接口方法,用**多组测试 case** 分别跑 SQLite 与 MemoryLake,**比对结果正确性**;当两者不同,依既定 rubric **判定谁更好**并记录。

---

## 1. 为什么不能只做「精确相等」断言

两后端在设计上就有本质差异,直接 `assert equal` 会全红且无意义:

| 差异源 | 表现 | 可比性 |
|---|---|---|
| MemoryLake 写入经 LLM 抽取 | 1 条 observation → N 条 fact、措辞改写 | fact 文本**不可精确比**;但 `engram_raw` 逐字原文**可精确比** |
| 检索:BM25 关键词 vs 语义向量 | 命中集合/排序不同 | 用**指标**(recall@k / MRR / 命中集合 IoU)+ 人/LLM 评判 |
| 写入异步(~12s) | 写后不立即可见 | 比对前需**等待收敛**(轮询到稳定) |
| id 模型不同 | int64(SQLite)vs int64↔fact-id 映射(MemoryLake) | 只比**语义身份**(同一条内容),不比 id 数值 |
| pinned/relations 语义重定义 | 共享 pin、conflict API | 比**行为等价**,非实现等价 |

因此每个方法先归入一种**比对模式**(§3),再定义 case。

---

## 2. 测试台架(harness)设计

### 2.1 结构
- 新增 `internal/paritytest/`(带 `//go:build parity` tag,**不进默认 `go test ./...`**,单独 job 跑,因需真实 MemoryLake + LLM,慢且花钱)。
- 每个 case 定义一次,driver 分别对两个后端执行:
  ```go
  type Backend interface{ mcp.MemoryBackend } // 复用生产接口
  func runCase(t *testing.T, c Case) Result    // 对单后端跑
  // 对比:res_sql := runCase(sqlite, c); res_ml := runCase(memorylake, c)
  //       verdict := compare(c.Mode, res_sql, res_ml)
  ```
- **隔离**:SQLite 用临时 db 文件(`t.TempDir()`);MemoryLake 用**临时 project**(`engram-parity-<uuid>` 挂 engram workspace),case 结束 forget facts + delete project + unbind actor(参照本会话验证脚本的清理流程)。
- **异步收敛**:MemoryLake 写后,driver 轮询 `facts` 列表直到数量稳定(连续 2 次不变)或超时 `EXTRACT_MAX_WAIT_MS`,再做读比对。

### 2.2 数据集(fixtures)
统一一组贴近 Engram 真实用途的 observation 语料 `testdata/corpus.jsonl`,覆盖:
- 决策类(`type=decision`,含精确标识:函数名/命令/版本号/报错串)。
- bugfix(含根因 + 代码符号)。
- convention/pattern(自然语言为主)。
- 多语言(中/英/西,呼应 `mem_capture_passive` 的西语解析)。
- 长文本(超 `MaxObservationLength`,验证截断一致性)。
- 近义/冲突对(供 conflict/judge 比对)。
- 同 topic_key 多次写(供 upsert 比对)。
每条含人工标注:`key_facts`(必须保留的要点)、`gold_query→relevant_ids`(检索金标准)。

---

## 3. 比对模式与「谁更好」rubric

| 模式 | 适用 | 正确性判据 | 「更好」判据(当不同) |
|---|---|---|---|
| **EXACT** | 逐字应一致的字段(`GetObservation.content` 取 `engram_raw`;title/type/scope/topic_key) | 字符串完全相等 | 不应不同;若不同 = MemoryLake **缺陷**(原文保真破损),SQLite 胜 |
| **SEMANTIC** | fact 抽取文本、summary | LLM-judge:是否语义等价且 `key_facts` 全含 | 谁更完整/无臆造/无丢关键要点者胜(LLM-judge 打分 0–5 × 2 独立评审取均值) |
| **SET/RANK** | Search、context、review 列表、timeline | 相对 gold 标注算 recall@k、precision@k、MRR、命中集合 IoU | 指标高者胜;并列时人工抽检 top-5 相关性 |
| **BEHAVIOR** | pin/unpin、session_*、delete(软删)、save 幂等/dedupe | 行为后置状态等价(再读一致) | 行为一致=平;不一致按是否符合工具契约判缺陷方 |
| **UNSUPPORTED** | merge_projects、hard_delete、doctor(SQLite 专项) | MemoryLake 返回明确「不支持」错误即为正确 | 不比优劣;仅断言降级契约 |
| **LATENCY** | 所有写/读 | 记录 P50/P95 耗时 | 供参考,不作正确性判据(异步是已接受代价) |

### 3.1 LLM-judge 协议(SEMANTIC / 排序抽检)
- 输入:原始 observation、SQLite 结果、MemoryLake 结果、`key_facts`。
- 输出 JSON:`{sqlite_score, memorylake_score, missing_key_facts[], hallucinations[], winner: sqlite|memorylake|tie, reason}`。
- **2 个独立评审**(不同 prompt 视角:①完整性 ②无臆造/保真),分歧则第 3 位仲裁。
- judge 用最强可用模型;prompt 固化在 `testdata/judge-prompt.md` 以可复现。

### 3.2 汇总记分卡
每方法输出一行:`method | cases | exact_pass | metric(sql vs ml) | winner分布 | ml_defects | 结论(≥/=/<)`。
**总体判定 #5**:所有 EXACT 项 100% 通过 **且** SET/RANK 项 MemoryLake 指标 ≥ SQLite × (1 − ε)(ε 默认 0.05,可配),记为「不低于」;否则列出低于项供决策。

---

## 4. 逐接口测试矩阵

> 每方法 ≥3 个 case;列出输入要点、比对模式、预期是否分歧、「更好」关注点。

### 4.1 写入 / 读取
| 方法 | Cases(≥3) | 模式 | 预期分歧 | 更好关注点 |
|---|---|---|---|---|
| **AddObservation** | ①普通 decision ②同 topic_key 二次写(upsert)③重复内容(dedupe)④超长截断 | BEHAVIOR + EXACT(回读 `engram_raw`) | MemoryLake 异步、可能 1→N;upsert 走 PATCH | 回读原文必须 EXACT;upsert 后是否仅 1 条逻辑记录 |
| **GetObservation** | ①存在 ②软删后取 ③不存在 | EXACT(content=engram_raw)+ BEHAVIOR | content 应一致;软删后均不可见 | 若 MemoryLake content≠原文 → 缺陷 |
| **UpdateObservation** | ①改 content ②只改 type ③改 topic_key | EXACT + BEHAVIOR | metadata 整体替换需先读合并 | 未传字段是否被误清空 |
| **DeleteObservation** | ①软删 ②`hard_delete=true` ③删不存在 | BEHAVIOR + UNSUPPORTED(硬删) | MemoryLake 无硬删 | 硬删是否明确降级为软删并告知 |
| **Search** | ①精确标识符(函数名/报错串)②自然语言语义 ③topic_key `/` 直查 ④type 过滤 ⑤跨 project ⑥空结果 ⑦中文 query | SET/RANK | **重点分歧区**:BM25 vs 语义+fuzzy | 用 gold 标注比 recall@k/MRR;精确标识符场景是否靠 fact_fuzzy 追平 |
| **Timeline** | ①中间锚点 ②边界锚点 ③before/after 不同窗口 | SET/RANK | id序 vs created_at序 | 前后邻居是否与 SQLite 同序 |
| **FormatContext** | ①有 pin ②无 pin ③多 session | SET/RANK | pin 客户端过滤 | pinned 是否正确置顶、近期 session 是否齐 |
| **Stats** / **CountObservationsForProject** | ①空库 ②多 project ③软删后计数 | SET(数值) | 计数来源不同 | 计数是否一致(软删是否都排除) |

### 4.2 pin / review / session / prompt / passive
| 方法 | Cases(≥3) | 模式 | 关注点 |
|---|---|---|---|
| **PinObservation/UnpinObservation** | ①pin 后 context 置顶 ②unpin 复原 ③pin 不存在 | BEHAVIOR | pin 后再读 pinned=true;语义变共享是否符合设计 |
| **ObservationsNeedingReview/MarkReviewed** | ①刚写不需 review ②过期需 review ③mark 后重置 | BEHAVIOR + SET | 客户端衰减是否与 SQLite review_after 同判 |
| **CreateSession/GetSession/EndSession/MostRecentActiveSession/RecentSessions** | ①开-读-结 ②最近活跃 ③最近 N | BEHAVIOR + SET | session↔conversation 映射行为等价 |
| **AddPrompt/AddPromptIfMissing** | ①新 prompt ②重复(missing 应跳过)③空 | BEHAVIOR | 幂等去重是否等价 |
| **PassiveCapture** | ①含 `## Key Learnings` 英 ②西语 ③无可抽取 ④含重复 | SEMANTIC + BEHAVIOR | extracted/saved/duplicates 计数 + 抽取要点保真 |

### 4.3 项目 / 关系(近似映射,重点观察)
| 方法 | Cases(≥3) | 模式 | 关注点 |
|---|---|---|---|
| **ProjectExists/ListProjectNames** | ①已知 ②未知 ③大小写变体 | BEHAVIOR + SET | 存在性判定一致 |
| **MergeProjects** | ①合并 ②源不存在 | UNSUPPORTED | MemoryLake 返回明确不支持错误 |
| **FindCandidates** | ①有近似候选 ②无候选 ③跨 project 应排除 | SET | mem_save 冲突候选:MemoryLake 用 conflict API,比候选召回是否可用 |
| **JudgeRelation/JudgeBySemantic** | ①supersedes ②conflicts_with ③not_conflict(no-op)④跨 project 拒绝 | BEHAVIOR | 6-verb→conflict resolve 近似映射是否达成等价后置状态;不可达项须记录为「已知差距」 |

---

## 5. 报告与判定
- 每次运行产出 `parity-report-<date>.md`:§3.2 记分卡 + 每个 EXACT 缺陷的最小复现 + 每个 SET/RANK 的指标表 + LLM-judge 分歧明细。
- **门槛**:EXACT 项须 100% 通过(否则原文保真破损,必修);SET/RANK 项汇总须满足 `ml ≥ sql×(1−ε)`;UNSUPPORTED 项须全部返回契约化错误。未达门槛的方法逐条列出,交人决策(接受重定义 / 改进映射 / 该 project 不启用 MemoryLake)。
- 该报告是 #5 的验收证据,也是 spec §11 各 spike(抽取延迟、1→N 可控性、fact_fuzzy 精度、conflict 时机)的实测出口。

---

## 6. 落地位置(实现计划挂接)
- 该测试台架作为**实现计划 Task 11 的增强**:新增 `internal/paritytest/`(`//go:build parity`)、`testdata/corpus.jsonl`、`testdata/judge-prompt.md`、CI 单独 `parity` job(手动/夜间触发,需注入 `ENGRAM_MEMORYLAKE_API_KEY`)。
- 依赖 Task 9(`MemoryLakeBackend`)与 Task 1(`MemoryBackend` 接口)完成后方可运行;可在 Task 9 完成即先跑「写/读/检索」子集做早期信号。
