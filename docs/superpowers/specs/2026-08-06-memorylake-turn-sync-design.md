# 设计：MemoryLake 逐轮对话同步（per-turn conversation sync）

- 日期：2026-08-06
- 状态：待评审
- 作用范围：**仅** MemoryLake 后端项目中显式开启本开关的项目。未开启的项目（含所有 SQLite 项目）行为完全不变。
- 前提：`internal/memorylake` 薄适配器已落地（见 `2026-07-23-memorylake-thin-adapter-design.md`），`mem_save` 走直写 fact 端点，`mem_save_prompt` 走 conversation append。

## 1. 目标与动机

Engram 当前把记忆的**产出时机**完全交给模型：模型判断"这值得记"，才调 `mem_save`。这条路径漏记严重——模型在长任务里常常忘记保存，而真正有价值的决策往往散落在对话过程中，而非结束时的总结里。

MemoryLake 的 conversation 抽取管线（mem0/fact）本来就是"喂原始对话、后台自动产出记忆"的设计。本功能把每一轮问答在轮次结束的瞬间写进 MemoryLake conversation，让**记忆的产出不再依赖模型的自觉**：

- 模型不需要判断什么值得记，也不需要多花一次工具调用
- 抽取、去重、矛盾合并全部由 MemoryLake 后台完成，Engram 不轮询、不回填
- 产出的 fact 经既有读路径（`mem_search` / `mem_context` / `Timeline`）自然可见

### 非目标

- **不替代 `mem_save`。** 模型主动保存的显式决策仍走直写 fact 端点，语义（逐字保真、可指定 title）不变。
- **不做本地镜像。** 逐轮同步的内容只存在于 MemoryLake，本地 SQLite 不留副本。
- **不覆盖非 Claude Code 的 agent。** 见 §14。
- **不做补偿投递。** 失败即丢弃，见 §12。

## 2. 已确认的决策

| # | 决策 | 取舍 |
|---|---|---|
| D1 | 开关**依附**于现有 `memorylake enable`：只有已启用 MemoryLake 后端的项目才能再开逐轮同步 | 复用现成的 workspace / proj_id / actor 解析；SQLite 项目用不了这个功能 |
| D2 | 每轮只写**用户提问 + 助手最终回复**，不含 thinking / tool_use / tool_result | 信噪比与抽取质量最优、体量可控、泄密面最小；丢失"改了哪些文件"这类行为轨迹 |
| D3 | 一轮写成**一条合并消息**，用现有单一 HUMAN actor 发送 | 零 MemoryLake 侧改动，问答天然成对不会被抽取批次切开；云端 `role` 全是 `USER`，dashboard 上区分不出说话人 |
| D4 | 开关打开时，**抑制** `appendPrompt` 对 conversation 的追加 | 避免用户提问在云端出现两次；因 prompts 在 MemoryLake 后端是只写不读，本地零损失 |
| D5 | 传输路径 = **Stop hook → 新 CLI 子命令 `engram turn`** | 不依赖 `engram serve` 是否在跑，解析逻辑在 Go 里可测；每轮多一次进程启动（`async: true` 下不阻塞） |
| D6 | 失败**发完就忘** + 超长截断 | 实现与测试面最小；断网期间的对话永久丢失 |

### D3 的背景约束

MemoryLake 消息的 `role` 由 actor 的 `actor_type` 推导（`HUMAN→USER`、`ASSISTANT→ASSISTANT`），而 `POST /actors` **只能创建 HUMAN** —— ASSISTANT actor 只能随 Agent 由系统创建（见 `memorylake-backend/docs/v3-workspace/v3-api-detailed-design.md` §6.1、§7.1）。`internal/memorylake/identity.go` 的 `EnsureActor` 因此固定写 `actor_type: "HUMAN"`。要让助手消息带上正确的 `ASSISTANT` role，必须先在 MemoryLake 侧建 Agent 并把其 actor id 作为新配置项传进来——引入跨仓依赖和一个必须手工完成的前置步骤。本设计选择在合并消息的文本里用 `**User:**` / `**Assistant:**` 标注说话人，把角色信息交给下游抽取用的 LLM 去读。

## 3. 组件与边界

### 新增

| 位置 | 职责 |
|---|---|
| `internal/turncapture/`（新包） | 纯解析层：读 Claude Code transcript JSONL，输出 `Turn{SessionID, UserText, AssistantText}`，并负责合并文本的渲染与截断（`Turn.Merged`）。零网络、零 MemoryLake 概念、零 store 依赖 |
| `internal/memorylake/turns.go` | `(*MemoryLakeBackend).AppendTurn`：复用现有 `client.AppendObservation` 写入 conversation |
| `cmd/engram/turn.go` | `engram turn` 子命令（`cmdTurn`） |
| `plugin/claude-code/scripts/turn-capture.sh` | Stop hook 脚本：搬运 stdin 字段并调 CLI |

`turncapture` 独立成包而不是塞进 `internal/memorylake`：解析 Claude Code transcript 属于 **agent 适配**领域，与 MemoryLake 无关。未来接 Codex / OpenCode 只需在这里新增一个 parser，`internal/memorylake` 与 `cmd/engram` 不动。这也是 `architecture-guardrails` 的边界判定结论：本地/协议解析 → 独立包；MemoryLake 写入 → `internal/memorylake`。

### 改动（均为加字段 / 加分支，不改现有函数签名）

| 位置 | 改动 |
|---|---|
| `internal/memorylake/config.go` | `ProjectEntry` 新增 `SyncConversations bool`；新增 `Enablement.IsConversationSyncEnabled` / `SetConversationSync` |
| `internal/memorylake/backend.go` | `MemoryLakeBackend` 新增 `skipPromptAppend bool` 字段 + `SetSkipPromptAppend(bool)` 方法 |
| `internal/memorylake/prompts.go` | `appendPrompt` 在 `skipPromptAppend` 为真时直接返回 `contentHash(p.Content)`，不发请求 |
| `cmd/engram/routing.go` | 构造 backend 后按 `entry.SyncConversations` 调 `SetSkipPromptAppend` |
| `cmd/engram/main.go` | `main` 的 switch 新增 `case "turn"`；`cmdMemorylake` 新增 `case "conversations"`；`printUsage` / `printMemorylakeUsage` 相应补行 |
| `plugin/claude-code/hooks/hooks.json` | `Stop` 数组新增第二个条目 |

`SetSkipPromptAppend` 用 setter 而不是改 `NewBackend` 签名：`NewBackend` 有大量既有调用点与测试，改签名的波及面远大于收益。

## 4. 开关：配置模型与 CLI

### 持久化

沿用 `~/.engram/memorylake.json`（`memorylake.DefaultEnablementPath()`），在既有 `ProjectEntry` 上加一个字段：

```go
type ProjectEntry struct {
    ProjID            string `json:"proj_id"`
    EnabledAt         string `json:"enabled_at"`
    SyncConversations bool   `json:"sync_conversations,omitempty"`
}
```

`omitempty` + bool 零值意味着**旧文件反序列化出来就是关闭态**，无需迁移、无需版本号。这是"不影响现有功能"最关键的一处：升级二进制后所有现存项目的行为逐字节不变。

新增两个方法：

```go
// IsConversationSyncEnabled 报告 project 是否既启用了 MemoryLake 后端、又开启了逐轮同步。
func (e *Enablement) IsConversationSyncEnabled(project string) bool

// SetConversationSync 开启/关闭 project 的逐轮同步。project 不在 EnabledProjects
// 中时返回错误 —— 逐轮同步依附于 MemoryLake 后端启用（D1）。
func (e *Enablement) SetConversationSync(project string, on bool) error
```

两者必须与既有 `IsEnabled` 用**完全相同**的键约定 —— 现在 `IsEnabled` 是对 `EnabledProjects` 的直接 map 查找，不做规范化（调用方传进来的已是 `project.DetectProjectFull` 的结果）。新方法照此办理：不要在这里引入 `store.NormalizeProject`，否则同一个项目在 `IsEnabled` 与 `IsConversationSyncEnabled` 下会命中不同的键。

### CLI 表面

```
engram memorylake conversations enable  --project <name>
engram memorylake conversations disable --project <name>
engram memorylake status          # 每个已启用项目后追加 conversations: on|off
engram turn --session <id> --transcript <path> --cwd <dir> [--verbose]
```

`conversations enable` 对未 `memorylake enable` 的项目报错并提示先启用后端 —— 把 D1 的依附关系做成硬边界，而不是留给文档去解释。

`engram turn` 在 `printUsage()` 中标注 `(internal, invoked by agent hooks)`：不鼓励手工调用，但也不隐藏。可发现性比藏起来更重要，调试与补录都要用它（见 §10）。

### 全局安全阀

`ENGRAM_BACKEND=sqlite` 已是全仓的 MemoryLake 总开关。`engram turn` **必须**尊重它：该环境变量为 `sqlite` 时立即 exit 0，不读 enablement 文件、不发任何请求。

## 5. 数据流

```
用户提交消息
  └─► UserPromptSubmit hook（不变）──► POST /prompts ──► backend.AddPromptIfMissing
                                                            └─ 开关开 → 不发 HTTP，返回 contentHash

模型回复结束
  └─► Stop hook（新增条目, async: true）──► turn-capture.sh ──► engram turn --session S --transcript T --cwd C
                                                                        │
                   ┌────────────────────────────────────────────────────┴───────────────────────────────┐
                   │ 1. ENGRAM_BACKEND=sqlite → exit 0                                                  │
                   │ 2. proj := detectProject(cwd)          // project.DetectProjectFull，尊重 .engram/config │
                   │ 3. enab := loadMemorylakeEnablement(~/.engram/memorylake.json)                     │
                   │ 4. !IsEnabled(proj) || !entry.SyncConversations → exit 0   ★ 最热路径，零网络零日志 │
                   │ 5. turn := turncapture.LastTurn(T)        // 解析失败 → 记日志, exit 0            │
                   │ 6. content, ok := turn.Merged(maxBytes)   // !ok → 静默 exit 0（常态，不记日志）   │
                   │ 7. backend := memorylake.NewBackend(loadMemorylakeConfig(), ws, entry.ProjID)       │
                   │ 8. backend.AppendTurn(turn.SessionID, content)  // 失败 → 记日志, exit 0           │
                   └────────────────────────────────────────────────────────────────────────────────────┘

MemoryLake 后台自动抽取 conversation → 生成 / 更新 / 合并 fact
  └─► 经既有读路径（mem_search / mem_context / Timeline）可见；Engram 不轮询、不回填
```

第 4 步是设计重心：**未开启的项目在这里就返回**——没有网络请求、没有日志写入、没有打开 SQLite、没有解析 transcript。整个进程只读了一个几百字节的 JSON 并做一次 map 查找。绝大多数项目跑的就是这条路，这是本功能"零影响"的落点。

第 7 步不复用 `cmd/engram/routing.go` 的 `resolveMemoryLakeBackend`：那个函数夹带了 SQLite 回退和把解析出的 proj_id 写回 enablement 文件的副作用，对逐轮同步都是多余的（失败就放弃，不回退）。直接用 `memorylake.NewBackend(cfg, ws, projID)`——`entry.ProjID` 在 `memorylake enable` 时已持久化，`ws` 来自 `ResolveWorkspaceID`。若 `entry.ProjID` 为空（老版本写的条目），exit 0，等下一次正常 `mem_save` 把它补上。

## 6. 轮次切分：`turncapture` 算法

### 接口

```go
package turncapture

// Turn 是一轮问答的可写入形态。UserText 或 AssistantText 为空时该轮不应写入。
type Turn struct {
    SessionID     string
    UserText      string
    AssistantText string
}

// LastTurn 解析 path 指向的 Claude Code transcript JSONL，返回其中最后一轮。
func LastTurn(path string) (Turn, error)

// Merged 把一轮渲染成待写入的合并消息文本（格式与截断见 §7）。
// ok == false 表示该轮不应写入：UserText 或 AssistantText 为空，
// 或 maxBytes 太小以致两部分都拿不到最低预算。
func (t Turn) Merged(maxBytes int) (content string, ok bool)
```

错误契约：路径不存在或不可读 → 返回 error。JSONL 中的坏行 → 跳过，**不**报错。扫到文件头仍未遇到轮起点 → 返回 `Turn`（`UserText` 为空）且 error 为 nil，由调用方判空放弃。

合并与截断放在 `turncapture` 而不是 `cmd/engram`：它们是纯字符串运算，放在这里能和解析共享同一套 fixture 做表驱动测试，`cmdTurn` 只负责编排。

### transcript 的真实形状（已在实际文件上验证）

一行一个 JSON 对象，相关字段：

```go
type entry struct {
    Type        string `json:"type"`        // user | assistant | attachment | queue-operation | system | summary | ...
    IsMeta      bool   `json:"isMeta"`      // true = 注入的上下文（skill 正文、系统提示），非人类输入
    IsSidechain bool   `json:"isSidechain"` // true = 子代理轮次
    SessionID   string `json:"sessionId"`
    Message struct {
        Role    string          `json:"role"`
        Content json.RawMessage `json:"content"` // string 或 []block
    } `json:"message"`
    Attachment struct {
        Type   string `json:"type"`   // queued_command | ...
        Prompt string `json:"prompt"`
    } `json:"attachment"`
}
```

三个容易踩的坑：

1. **工具回执也是 `type: "user"`**，其 `content` 数组里含 `tool_result` 块。把所有 `type=="user"` 当轮边界会导致每轮只截到最后一次工具调用之后的片段。
2. **中途插话不是普通 user 条目**，而是 `type=="attachment"` 且 `attachment.type=="queued_command"`，文本在 `attachment.prompt` 里（另有配套的 `type=="queue-operation"` 条目，重复同一内容，须忽略）。
3. **`isMeta: true` 的 user 条目**是注入的上下文（例如 skill 正文），不是人类输入，既不该采集也不该当轮边界。

### 反向扫描状态机

从最后一行往前遍历，维护 `userParts` 与 `assistantParts` 两个切片，每次 **prepend**（因为反向扫描，prepend 得到的就是时序顺序）：

| 条目判定 | 动作 |
|---|---|
| `json.Unmarshal` 失败 | **跳过**（transcript 是活文件，最后一行可能只写了一半） |
| `IsSidechain == true` | 跳过（子代理轮次不入库） |
| `Type == "assistant"` | 从 `content` 数组取 `type=="text"` 的 `text`，prepend 到 `assistantParts`；`thinking` / `tool_use` 丢弃 |
| `Type == "attachment" && Attachment.Type == "queued_command"` | prepend `Attachment.Prompt` 到 `userParts`（不停止扫描） |
| `Type == "user"` 且 `content` 数组含 `tool_result` 块 | 跳过（工具回执，不是轮边界） |
| `Type == "user"` 且 `IsMeta == true` | 跳过（注入上下文，不是轮边界） |
| `Type == "user"`（其余情况） | 取 `content`（string 直接用；数组取 `type=="text"` 拼接）prepend 到 `userParts`，**停止扫描** |
| 其他 `Type`（`queue-operation` / `system` / `summary` / `file-history-snapshot` …） | 跳过 |

扫描到文件头仍未遇到轮起点：返回目前收集到的内容（`UserText` 可能为空，由调用方判空放弃）。

### 文本清洗

对 `userParts` 每一段，整块剥除以下包裹标签及其内容：

```
<command-message>…</command-message>
<command-name>…</command-name>
<system-reminder>…</system-reminder>
<local-command-stdout>…</local-command-stdout>
```

然后 trim 空白，丢弃清洗后为空的段。

**斜杠命令的特例**：纯斜杠命令调用（如 `/superpowers:brainstorming` 不带参数）清洗后会一无所剩。此时取 `<command-name>` 的内容加 `/` 前缀作为用户文本（`/superpowers:brainstorming`），保留"这一轮是由哪个命令触发的"这一信息，而不是让整轮被丢弃。

最后用 `\n\n` 连接各段，得到 `UserText` 与 `AssistantText`。

### 大文件处理

长会话的 transcript 可达数十 MB，全量读入不合适：

- 文件 ≤ `ENGRAM_TURN_MAX_TRANSCRIPT_BYTES`（默认 64 MiB）：整读后按行反向遍历
- 超过时：只读尾部 `ENGRAM_TURN_TAIL_WINDOW_BYTES`（默认 2 MiB），丢弃窗口第一行（可能被切断），在窗口内反向定位轮边界；窗口内找不到轮起点则返回空 `UserText`，调用方放弃该轮

## 7. 合并消息格式与截断

```
**User:**
<UserText>

**Assistant:**
<AssistantText>
```

### 截断规则

上限 `ENGRAM_TURN_MAX_BYTES`（默认 32768），作用于最终合并文本的 UTF-8 字节数。超限时：

1. 扣除模板固定开销（`**User:**\n`、`\n\n**Assistant:**\n`）
2. 剩余预算按 `len(UserText) : len(AssistantText)` 比例分配，每部分下限 1024 字节（若总预算不足以给两部分各 1024，则整轮放弃并记一行日志）
3. 超预算的部分保留头部 60%、尾部 40%，中间插入 `\n…[truncated N bytes]…\n`
4. 所有切点对齐 UTF-8 rune 边界，绝不产出坏字节

保留头尾而非只保留头部：一轮对话的结论通常在末尾，只截头会把最有价值的部分丢掉。

## 8. 写入路径：`AppendTurn`

```go
// AppendTurn 把一轮合并后的对话文本作为一条消息追加到 sessionID 对应的
// MemoryLake conversation。复用 client.AppendObservation —— 即
// ensureConversation(custom_id = sessionID) + POST /conversations/{id}/messages
// 这条已有通路，不新增任何 HTTP 客户端代码。
//
// 消息的 custom_id 是 content 的 sha256 前 16 位（AppendObservation 内部行为），
// 所以对同一轮重复调用是幂等的：MemoryLake 把重复 custom_id 解析为已存在的消息，
// 不会产生第二条。
func (b *MemoryLakeBackend) AppendTurn(sessionID, content string) (string, error)
```

实现要点：

- 持 `b.writeMu`，与 `AddObservation` / `AddPrompt` 串行化（同一进程内不会并发，但保持一致）
- `convCustomID = sessionID`；`sessionID` 为空时退回 `defaultConversationCustomID`，与 `appendPrompt` 一致——这保证逐轮消息和该会话的其他消息落在**同一个** conversation 里
- 传给 `AppendObservation` 的 `store.AddObservationParams` 用 `Type: "turn"`、`Title: "Conversation turn"`。这两个字段在 conversation append 路径上不参与请求体构造（只有 `Content` 会），填写它们纯粹是为了让日志和将来的调试有意义

## 9. prompt 追加抑制

`MemoryLakeBackend` 新增字段与 setter：

```go
skipPromptAppend bool

// SetSkipPromptAppend 让 AddPrompt / AddPromptIfMissing 不再向 MemoryLake
// conversation 追加消息。逐轮同步开启时由 cmd/engram/routing.go 调用：合并
// 消息里已含用户提问，再单独追加一次会让同一句话在云端出现两遍，污染抽取。
// 安全的原因：在 MemoryLake 后端下 prompts 是只写不读的 —— backend.go 里没有
// 任何读回路径，FormatContext / Stats / Timeline 都不含 prompt 区块。
func (b *MemoryLakeBackend) SetSkipPromptAppend(v bool)
```

`appendPrompt` 的改动就是开头一行：

```go
if b.skipPromptAppend {
    return contentHash(p.Content), nil
}
```

返回 `contentHash` 而非空串：调用方（`mem_save_prompt`、`POST /prompts`）拿到的是一个稳定、非空的 id，语义与真实写入时一致，不需要为这个分支写任何特判。`AddPromptIfMissing` 的进程内去重缓存逻辑保持不变，`inserted` 仍然首次为 `true`。

## 10. `engram turn` CLI 契约

### 参数

三个值全部来自 Stop hook 的 stdin JSON，脚本只搬运不计算。

| flag | 来源 | 为什么不能省 |
|---|---|---|
| `--session <id>`（可选） | `.session_id` | conversation 的 `custom_id`。transcript 每行也有 `sessionId`，但 `--resume` / `/clear` 后 Claude Code 会改写归属关系，以 hook 给的为准更可靠。**未传时回退**到 transcript 末行的 `sessionId`（手工调用时的便利路径）；两者都无则放弃该轮。hook 脚本自己保证非空，所以在 CLI 层不做必填校验 |
| `--transcript <path>`（必填） | `.transcript_path` | 目录名那个 slug 是 Claude Code 把 cwd 做转写得来的（`/Users/x/engram` → `-Users-x-engram`），转写规则属于其内部实现，在 engram 里复刻一份就是在赌它永不改 |
| `--cwd <dir>`（可选） | `.cwd` | ① hook 子进程的工作目录由 Claude Code 决定，`os.Getwd()` 不可靠。② **不能在 bash 里算项目名**：`_helpers.sh` 的 `detect_project()` 只看 git remote 和目录名，而 Go 的 `DetectProjectFull` 会优先读 `.engram/config` 里的项目名（本仓库正走此路径）。bash 算出的名字遇到用 config 改过名的项目就会与 MCP 侧不一致，开关查表查空，功能静默失效 |
| `--verbose`（可选） | — | 打印一行 `appended turn to conversation <session-id> (message <id>, <n> bytes)`，供手工验证 |

flag 手工解析，与仓库其他子命令一致（无 flag 框架）。

### 退出码

| 情形 | 退出码 |
|---|---|
| 用法错误（缺 `--transcript`、未知 flag） | **2** + usage 到 stderr |
| 一切运行时情况（安全阀生效、项目未开启、transcript 缺失、解析失败、网络失败、云端 5xx、写入成功） | **0** |

用法错误报非零、运行时一律零，这个划分是刻意的：hook 脚本自己会 `|| true` 吞掉退出码，所以对用法错误报非零不会打扰用户；而手工调试时把 `--transcirpt` 拼错却静默返回 0，是会浪费掉半小时的那种坑。

stdout 默认**完全静默**——Stop hook 的输出在某些终端会闪现给用户。

### 手工调用

```bash
engram turn --verbose \
  --session a67b77b8-2be3-4051-805d-2cd6b6b1f77f \
  --transcript ~/.claude/projects/-Users-chuangxianwei-engram/a67b77b8-2be3-4051-805d-2cd6b6b1f77f.jsonl \
  --cwd ~/engram
```

因为 message 的 `custom_id` 是内容 hash，这条命令**可以反复跑**，同一轮不会产生第二条消息。这也让"补录"成为可能：对着某个历史 transcript 跑一遍，就能把当时那轮灌进去。

## 11. Hook 集成

`plugin/claude-code/hooks/hooks.json` 的 `Stop` 数组新增第二个条目（保留现有的 `session-stop.sh` 条目不动）：

```json
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
```

`${CLAUDE_PLUGIN_ROOT}` 必须带双引号 —— `plugin/hooks_quoting_test.go` 的 `TestHooksJSONPluginRootIsQuoted` 会检查。

`plugin/claude-code/scripts/turn-capture.sh`：

```bash
#!/bin/bash
# Engram — per-turn conversation sync for Claude Code (async)
#
# Feeds each completed turn (one user message + the assistant's final reply)
# into the project's MemoryLake conversation so MemoryLake's own extraction
# pipeline can turn it into memories. No-op unless the project has conversation
# sync explicitly enabled; `engram turn` decides that, not this script.

INPUT=$(cat)
command -v engram >/dev/null 2>&1 || exit 0

SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')
[ -n "$SESSION_ID" ] && [ -n "$TRANSCRIPT" ] || exit 0

engram turn --session "$SESSION_ID" --transcript "$TRANSCRIPT" --cwd "$CWD" \
  >/dev/null 2>&1 || true
exit 0
```

不需要 `&` 后台化 —— `hooks.json` 里已 `async: true`，Claude Code 不等它。`command -v engram` 让未安装二进制的机器直接静默跳过；旧版 engram 不认识 `turn` 子命令会走 `default` 分支返回非零，被 `|| true` 吞掉。

`plugin/assets_test.go` 用 `scripts/*.sh` glob 扫描，新脚本自动纳入其语言触发词检查，无需登记清单。Claude Code 的 hook 注册只存在于这个 `hooks.json`（`internal/setup` 只内嵌 OpenCode 插件），因此无需改 `internal/setup`。

## 12. 错误处理与日志

**永不因运行时问题非零退出**（§10 的退出码表）。

**日志**：仅失败时向 `~/.engram/logs/turn.log` 追加一行 —— RFC3339 时间戳、project、session id、错误。文件超过 1 MiB 时重建（不做多文件轮转，这是诊断日志不是审计日志）。成功时不写任何东西。

运行时失败**不写 stderr** —— Stop hook 的 stderr 在某些终端会闪现给用户。唯一写 stderr 的是用法错误（§10 退出码 2 的那一栏），那条路径只有手工调用才会走到。

**不重试、不落盘补偿。** 断网或云端故障期间的对话永久丢失。对话语料是"锤子而非账本"——丢一轮不致命，而 spool 队列会引入生命周期、清理、并发、体积上限一整套新问题，与本功能的价值不成比例。

**永不打扰用户。** 无论失败多少次，都不向对话注入任何 systemMessage、不改变 hook 的退出行为。用户想知道状况就去看 `turn.log` 或 `engram memorylake status`。

## 13. 环境变量

| 变量 | 默认 | 作用 |
|---|---|---|
| `ENGRAM_BACKEND` | — | 已有的全局安全阀；值为 `sqlite` 时 `engram turn` 立即 exit 0 |
| `ENGRAM_TURN_MAX_BYTES` | `32768` | 单条合并消息的 UTF-8 字节上限 |
| `ENGRAM_TURN_MAX_TRANSCRIPT_BYTES` | `67108864`（64 MiB） | 超过则改读尾部窗口 |
| `ENGRAM_TURN_TAIL_WINDOW_BYTES` | `2097152`（2 MiB） | 尾部窗口大小 |

解析沿用 `config.go` 已有的 `envInt` 惯例：非法值静默回退到默认。

## 14. 已知取舍与限制

必须写进 `DOCS.md`，不能只留在这份 spec 里：

1. **ESC 打断的轮次不入库。** 用户中断时 Claude Code 不触发 `Stop`。
2. **只覆盖 Claude Code。** Codex 虽然也有 `Stop` hook 且跑 bash，但其 hook 输入是否提供可解析的 transcript 未经验证，且不支持 `async: true`（须在 5s 超时内同步跑完）。OpenCode / Pi 走各自的 TS 插件事件，需要另一套等价实现。`turncapture` 的包边界为这些留了扩展位。
3. **云端 `role` 全是 `USER`**（D3）。说话人信息只存在于消息正文的 `**User:**` / `**Assistant:**` 标注里。
4. **断网期间的对话永久丢失**（D6）。
5. **不含工具调用轨迹**（D2）。"改了哪些文件、跑了哪些命令"不进入云端语料。
6. **子代理轮次不入库。** `isSidechain == true` 的条目全部跳过。
7. **`/clear` 换 conversation。** session id 变了就是新 conversation；`--resume` 沿用同一 session id 因而续同一个 conversation。
8. **compaction 无特殊处理。** 被压缩掉的轮次在压缩发生前已写入云端，不会重复；压缩后 transcript 里的 `summary` 条目按"其他 `Type`"跳过。

## 15. 测试策略

| 层 | 覆盖 |
|---|---|
| `internal/turncapture` | 表驱动 + 真实 transcript 片段 fixture：纯文本轮、含 `tool_use`/`thinking` 的轮、`queued_command` 插话、`isMeta` 条目、只含 `tool_result` 的 user 条目、assistant 多段 `text`、空 assistant、损坏的 JSONL 行、`isSidechain`、纯斜杠命令轮、`content` 为 string vs array、扫到文件头未遇轮起点、尾部窗口截断路径 |
| `internal/memorylake/turns_test.go` | `httptest`：恰好两次请求（create conversation + append message）；`CUSTOM_ID_CONFLICT` 走 GET 恢复；同内容二次调用 `custom_id` 一致；`sessionID` 为空时落到 `defaultConversationCustomID` |
| `internal/memorylake/config_test.go` | 旧 JSON 无 `sync_conversations` 字段 → 读出 `false`；enable/disable 往返持久化；对未 `memorylake enable` 的项目 `SetConversationSync` 报错；项目名规范化一致性 |
| `internal/memorylake/prompts_test.go` | `skipPromptAppend` 下**零 HTTP 请求**且返回稳定非空 id；`AddPromptIfMissing` 的 `inserted` 语义不变。这是"不影响现有功能"的回归锁 |
| `cmd/engram` | `cmdTurn`：未开启项目零网络调用且 exit 0；`ENGRAM_BACKEND=sqlite` 零网络调用；transcript 缺失 exit 0；缺必填 flag exit 2；`entry.ProjID` 为空时 exit 0。`memorylake conversations enable/disable` 的 CLI 往返与错误路径 |
| 截断 | 边界表：刚好等于上限、超一字节、多字节字符跨越切点、两部分预算均不足 1024 |
| e2e（`-tags e2e`） | 沿用 `delete_e2e_test.go` 风格：`enable` → `conversations enable` → `turn` → 列消息断言恰好一条合并消息；再跑一次 `turn` 断言仍为一条（幂等） |

**不加 `internal/paritytest` 用例。** 差分测试比对 SQLite 与 MemoryLake 的等价行为，而逐轮同步是 MemoryLake 独有能力，SQLite 侧没有对照物。

## 16. 文档更新（同 PR 内完成）

- `DOCS.md` 的 "MemoryLake Backend" 新增一节：开关命令、轮次定义、§13 环境变量表、§14 全部取舍
- `README.md` 与中文安装文档各补一句开关说明
- `plugin/claude-code/skills/memory/SKILL.md` **不改** —— 本功能对模型完全透明，不该占它的上下文

## 17. 落地顺序（依赖顺序，非实施计划）

1. `config.go` 字段 + `IsConversationSyncEnabled` / `SetConversationSync` + 测试
2. `memorylake conversations enable|disable` CLI + `status` 显示 + 测试
3. `internal/turncapture` 包 + fixture 测试
4. `internal/memorylake/turns.go` 的 `AppendTurn` + `httptest`
5. `skipPromptAppend` 字段 + setter + `routing.go` 接线 + 回归测试
6. `cmd/engram` 的 `turn` 子命令 + 测试
7. `turn-capture.sh` + `hooks.json` 条目
8. e2e 用例
9. 文档

## 18. 仓库规则提醒

- 本功能落地需要一个带 `status:approved` 标签、恰好一个 `type:*` 标签的 issue，PR 里 `Closes #N`
- commit 用 `feat(memorylake):` 前缀；**不加** `Co-Authored-By` trailer
- CI 只跑 `go test ./...` 与 `go test -tags e2e ./internal/server/...`，无独立 lint 步骤
