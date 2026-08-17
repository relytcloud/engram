# Engram 安装与接入指南(中文)

Engram 是给 AI 编码 agent 用的**持久记忆**服务:单个 Go 二进制,内置 SQLite(默认、真源)+ 可选 MemoryLake 后端,通过 MCP(stdio)、CLI、HTTP、TUI 四种接口对外。本文覆盖 **macOS 和 Linux** 的安装,以及**如何把它接入 Claude Code 作为插件**。

> 版本:以 [Releases](https://github.com/relytcloud/engram/releases) 页最新的 `vX.Y.Z` 为准,下文示例用 `v0.2.0`。

---

## 一、安装二进制

有两种方式:**下载预编译包(推荐)** 或 **从源码编译**。

### 方式 A:下载预编译 release 包(推荐)

release 包对每个平台都是一个**静态链接、无依赖**的二进制(`CGO_ENABLED=0`,纯 Go SQLite),下载解压即用。

先确认你的系统架构:

```bash
uname -sm
# Darwin arm64  → macOS Apple Silicon (M 系列)
# Darwin x86_64 → macOS Intel
# Linux  x86_64 → Linux amd64
# Linux  aarch64→ Linux arm64
```

对照下表选择包名(`<os>_<arch>`):

| 系统 | 架构 | 包名 |
|---|---|---|
| macOS Apple Silicon | `Darwin arm64` | `engram_0.2.0_darwin_arm64.tar.gz` |
| macOS Intel | `Darwin x86_64` | `engram_0.2.0_darwin_amd64.tar.gz` |
| Linux x86_64 | `Linux x86_64` | `engram_0.2.0_linux_amd64.tar.gz` |
| Linux ARM64 | `Linux aarch64` | `engram_0.2.0_linux_arm64.tar.gz` |

#### macOS(Apple Silicon 示例)

```bash
cd /tmp
VER=0.2.0
curl -fsSL -o engram.tar.gz \
  "https://github.com/relytcloud/engram/releases/download/v${VER}/engram_${VER}_darwin_arm64.tar.gz"
tar -xzf engram.tar.gz engram
mkdir -p ~/.local/bin
install -m 0755 engram ~/.local/bin/engram

# macOS 二进制是 ad-hoc 签名(未公证),首次运行如被 Gatekeeper 拦截:
xattr -d com.apple.quarantine ~/.local/bin/engram 2>/dev/null || true
```

> Intel Mac 把上面的 `darwin_arm64` 换成 `darwin_amd64`。

#### Linux(amd64 示例)

```bash
cd /tmp
VER=0.2.0
curl -fsSL -o engram.tar.gz \
  "https://github.com/relytcloud/engram/releases/download/v${VER}/engram_${VER}_linux_amd64.tar.gz"
tar -xzf engram.tar.gz engram
mkdir -p ~/.local/bin
install -m 0755 engram ~/.local/bin/engram
```

> ARM64 机器(如树莓派 / 云 ARM 实例)把 `linux_amd64` 换成 `linux_arm64`。
> 想装到系统级 `/usr/local/bin` 就把 `~/.local/bin` 换成 `/usr/local/bin` 并加 `sudo`。

#### (可选)校验完整性

```bash
curl -fsSL -O "https://github.com/relytcloud/engram/releases/download/v${VER}/checksums.txt"
shasum -a 256 -c checksums.txt --ignore-missing   # macOS
# sha256sum -c checksums.txt --ignore-missing      # Linux
```

### 方式 B:从源码编译

需要 Go 1.25+。产物与 release 包一致(纯 Go,无需 CGO)。

```bash
git clone https://github.com/relytcloud/engram.git
cd engram
CGO_ENABLED=0 go build -o ~/.local/bin/engram ./cmd/engram
```

> 一份源码可交叉编译到所有平台,例如为 Linux amd64 出包:
> `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o engram-linux-amd64 ./cmd/engram`

---

## 二、把 `~/.local/bin` 加入 PATH 并验证

```bash
# 若还没加过,按你的 shell 追加一次(zsh 是 macOS 默认):
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc   # zsh
# echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc  # bash
exec $SHELL          # 或重开终端

# 验证:
engram version       # 应打印 0.2.0
which engram         # 应指向 ~/.local/bin/engram
```

首次运行任何命令会自动在 `~/.engram/engram.db` 创建本地数据库,这就是你的记忆真源。

---

## 三、接入 Claude Code(创建插件)

Engram 以 **Claude Code 原生插件**形式接入:一条命令帮你完成「注册插件市场 → 安装插件 → 写入 MCP 配置」。插件带来的能力:

- **MCP 记忆工具**(`mem_save` / `mem_search` / `mem_context` 等)——agent 直接调用;
- **hooks**:会话开始自动加载记忆协议、压缩(compaction)后自动恢复上下文;
- **skills**:记忆使用规范。

### 前置条件

已安装 Claude Code CLI(命令 `claude` 在 PATH 中)。没有的话先装:<https://docs.anthropic.com/en/docs/claude-code>

### 一键接入(推荐)

```bash
engram setup claude-code
```

它会自动做三件事:

1. `claude plugin marketplace add relytcloud/engram`(注册插件市场,幂等);
2. `claude plugin install engram@engram`(安装插件:hooks + skills);
3. 写入用户级 MCP 配置 `~/.claude/mcp/engram.json`,内容形如:

   ```json
   {
     "command": "/Users/you/.local/bin/engram",
     "args": ["mcp", "--tools=agent"]
   }
   ```

   这里用的是**二进制绝对路径**(而非 PATH 查找),所以即使 PATH 没传到 MCP 子进程(Windows 常见)也能启动;二进制移动位置后**重跑 `engram setup claude-code`** 即可修正。

完成后**重启 Claude Code**,让它加载插件与 MCP。

### 验证插件已生效

新开一个 Claude Code 会话,确认能看到 `mem_*` 工具(如让它调用 `mem_context`)。或用 CLI 侧验证记忆链路本身正常:

```bash
engram save "第一条测试记忆" --project demo
engram search "测试" --project demo
```

### 手动接入(不想用市场插件时)

只想要 MCP 记忆工具、不装 hooks/skills,可以直接注册 MCP server:

```bash
# 用 Claude Code 内置命令注册(用户级),注意 engram 换成绝对路径更稳:
claude mcp add engram -- "$(which engram)" mcp --tools=agent
```

或手写 `~/.claude/mcp/engram.json`(内容同上一节的 JSON)。

### 移除

```bash
claude plugin uninstall engram        # 卸插件
rm -f ~/.claude/mcp/engram.json       # 删 MCP 配置
```

---

## 四、(可选)启用 MemoryLake 后端

默认所有 project 都用**本地 SQLite**。只有你**显式 enable** 的 project 才会改用 MemoryLake(记忆以 V3 fact 形式存到 MemoryLake 的 `engram` workspace),其余 project 完全不受影响。二者可在同一台机器上并存。

### 1) 配置连接

**推荐:用 `engram memorylake config` 命令**(持久化到 `~/.engram/memorylake.json`,文件权限 `0600`,含 API key),一次配置,CLI 和 Claude Code 拉起的 MCP 子进程都能读到,不用每个 shell 都 export:

```bash
# base_url 不传就默认 https://app.memorylake.ai/openapi/memorylake
engram memorylake config --api-key "sk-你的APIKey"

# 需要时可一并指定:
engram memorylake config --url "https://app.memorylake.ai/openapi/memorylake" \
                         --api-key "sk-你的APIKey" --workspace engram

engram memorylake config           # 无参数 = 查看当前生效配置(api_key 会掩码显示)
engram memorylake config --clear   # 清空已保存的连接配置
```

**优先级:环境变量 > 上面保存的配置 > 内置默认**。所以 CI / 临时覆盖仍可用环境变量:

```bash
export ENGRAM_MEMORYLAKE_BASE_URL="https://app.memorylake.ai/openapi/memorylake"
export ENGRAM_MEMORYLAKE_API_KEY="sk-你的APIKey"
export ENGRAM_MEMORYLAKE_WORKSPACE="engram"     # 默认 engram
export ENGRAM_MEMORYLAKE_ACTOR="$(hostname)"    # 多人共享 project 时区分贡献者;默认取主机名
```

> API key 决定了写入的租户(tenant),Engram 代码里没有独立的 "org" 概念。
> `--url` / `ENGRAM_MEMORYLAKE_BASE_URL` 必须带 `/openapi/memorylake` 前缀,客户端会在其后拼 `/api/v3/...`。

### 2) 对某个 project 启用 / 关闭 / 查看

```bash
engram memorylake enable  --project <project名>   # 该 project 改用 MemoryLake
engram memorylake disable --project <project名>   # 改回本地 SQLite
engram memorylake status                          # 列出所有已知 project 及其当前后端
```

enable 信息记录在 `~/.engram/memorylake.json`;首次对该 project 做 `mem_save` 时会在 `engram` workspace 下按需创建对应 MemoryLake project(多人并发创建有 409 恢复,不会冲突)。

**首次 enable 会自动迁移已存记忆**:第一次对某 project 执行 `enable` 时,engram 会把它在本地 SQLite 里**未删除**的观测**逐字**写入 MemoryLake —— 走的是 MemoryLake 的**直接 add-fact 接口**(`POST …/memories/facts`,逐字入库、返回真实 fact id,**不经异步抽取**),所以迁移的记忆保真、**立即可检索**,且 **title 会保留**(拼在正文前)。SQLite 仍是真源,**本地不删任何东西**。

- 该接口自身不去重,所以迁移会先列出项目已有 fact、**跳过文本已存在的观测**,从而**幂等**——可重复跑,只补写新内容。
- `--no-migrate`:跳过这次自动迁移。
- `--migrate`:即使是重复 enable 也强制再同步一次(幂等)。
- 若此时还没配 API key 或网络失败,**项目仍会 enable**,只打印告警,修好后重跑 `engram memorylake enable --project <名> --migrate` 即可。

### 3) 全局安全阀

临时想让**所有** project 都强制走 SQLite(无视 enable 列表),设一个环境变量即可,便于回退排查:

```bash
export ENGRAM_BACKEND=sqlite
```

### MemoryLake 后端的已知差异

- `mem_save` 走 MemoryLake 时是**逐字直写、同步返回**:内容通过 MemoryLake 的 add-fact 接口逐字入库,立即返回**真实 fact id**、**立即可检索**,并**保留 title**(拼在正文前)。PassiveCapture 和 session-end summary 同路径;prompt(`mem_save_prompt`)仍走对话-append(prompt 不是 memory,且 add-fact 无 metadata,写成 fact 会污染 `mem_search`)。
- 搜索是**纯语义**(向量),不再是 SQLite 的 FTS5/BM25 精确匹配;但内容**逐字保真**(不再被 mem0 抽取/改写)。
- **取舍**:直写接口**不做去重 / topic_key upsert / 冲突决策** —— 每次 `mem_save` 都是一条新 fact,`topic_key` 在 MemoryLake 项目上不再是 in-place upsert(迁移时会对已存 fact 去重,但 live save 不会)。

已启用 MemoryLake 的项目还可以开启逐轮对话同步:

```bash
engram memorylake conversations enable --project <项目名>
```

开启后每完成一轮问答都会自动写入 MemoryLake,由云端抽取成记忆,不再依赖 agent 主动保存。

启用前务必了解以下几点(完整列表见 `DOCS.md` 的 "Per-turn conversation sync" 一节):

- **一旦开启,你输入的一切都会被逐字、自动上传 —— 没有本地副本,也无法撤回。** 合并进某一轮的每一句用户输入(包括不小心粘进 prompt 里的密钥)都会在这一轮结束的瞬间离开这台机器、写进 MemoryLake,本地不留副本,事后也无法收回。
- **切换开关需要重启 agent —— 无论开启还是关闭。** `cmd/engram/routing.go` 在进程生命周期内为每个 project 只缓存一个 MemoryLake backend 实例,prompt 追加的抑制标志也只在该 backend 构造时决定一次。如果在一个长期运行的 `engram mcp`(也就是你正在用的、已经启动的 agent)还活着时执行 `conversations enable`,那个进程会继续用旧的 backend 实例、抑制仍然是关闭的 —— 于是你的 prompt 会被重复追加,和合并后的整轮内容一起在 conversation 里出现两次。反过来,对一个存活进程执行 `conversations disable` 会是相反的问题:抑制会一直保持开启,该会话的 prompt 不再进入 conversation,而逐轮同步既然已关闭,`engram turn` 也不再写入任何东西 —— 这个会话在重启前对 MemoryLake 毫无贡献。**修复方法:切换开关后重启 agent**,无论朝哪个方向切换都一样。
- **不采集工具调用本身,但引用其内容的文本仍会上传。** `tool_use` / `tool_result` 这类工具调用/工具输出块不会进入合并后的消息。但助手的最终回复经常会在正文里逐字引用文件内容或命令输出,这部分文本属于会被上传的内容 —— 这条路径只保证不上传原始的工具调用/工具结果块,不保证文件或命令内容绝不离开这台机器。
- 仅支持 Claude Code;被 ESC 打断的轮次不会入库。

---

## 五、常见问题

| 现象 | 处理 |
|---|---|
| `engram: command not found` | `~/.local/bin` 没进 PATH,见第二节;或重开终端 |
| macOS 提示「无法验证开发者 / 已损坏」 | `xattr -d com.apple.quarantine ~/.local/bin/engram`,或右键→打开 |
| `engram setup claude-code` 报 `claude CLI not found` | 先装 Claude Code 并确保 `claude` 在 PATH |
| Claude Code 里看不到 `mem_*` 工具 | 重启 Claude Code;检查 `~/.claude/mcp/engram.json` 里的 `command` 是否为有效绝对路径;二进制移动过就重跑 setup |
| 启用 MemoryLake 后存进去搜不到 | 抽取是异步的,稍等;或用 `ENGRAM_BACKEND=sqlite` 对比验证链路 |
| 想彻底回到本地存储 | `engram memorylake disable --project <名>`,或全局 `export ENGRAM_BACKEND=sqlite` |

---

## 参考

- 完整 API / Schema / CLI / 环境变量:仓库根 `DOCS.md`(MemoryLake 一节)
- 发布流程:`RELEASING.md`
- 架构总览:`docs/ARCHITECTURE.md`、`docs/CODEBASE-GUIDE.md`
