# Engram 安装指南(中文)

Engram 是给 AI 编码 agent 用的持久记忆服务。本文只保留必须操作:**安装**和**开启 MemoryLake project 功能**。

## 一、安装

一条命令完成「二进制安装 + Claude Code 插件接入」(macOS / Linux,自动使用最新版本;升级 = 重跑同一条命令):

```bash
curl -fsSL https://raw.githubusercontent.com/relytcloud/engram/main/install.sh | bash
```

若提示 `engram: command not found`,把 `~/.local/bin` 加入 PATH:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc   # bash 用户改 ~/.bashrc
exec $SHELL
```

验证并生效:

```bash
engram version    # 打印版本号即安装成功
```

然后**重启 Claude Code**,新会话里能看到 `mem_*` 工具即接入成功。

## 二、开启 MemoryLake project 功能

**什么是 project**:project 是记忆的隔离单位——每条记忆归属一个 project,保存、搜索、加载上下文都只在该 project 范围内进行,MemoryLake 也是按 project 逐个开启。

**project 名怎么得出**:不显式指定时,engram 根据当前目录自动检测,通常就是 **git 仓库名**(优先取 git remote 里的仓库名,没有 remote 就用仓库根目录名;不在 git 仓库里则用当前目录名),并统一转为小写。例如在 `~/code/Engram` 仓库里工作,project 就是 `engram`。也可以在命令里用 `--project <名>` 显式指定;用 `engram projects list` 可查看本机已有的所有 project。

默认所有 project 用本地 SQLite。只有显式 enable 的 project 才改用 MemoryLake,其余不受影响。

### 1) 获取 API key

1. 打开 [MemoryLake 控制台](https://app.memorylake.cn) 并登录;
2. 点击左上角团队切换器,选择**「质变科技」**团队(API key 决定记忆写入哪个团队/租户,选错团队则记忆互不可见);
3. 进入 **API Keys** 页面,点击 **Create API Key**;
4. **立即复制保存**生成的密钥(`sk_` 开头)——密钥只在创建时显示一次,之后无法再查看。

### 2) 配置 API key(一次即可)

```bash
engram memorylake config --api-key "sk_你的APIKey"
```

> 服务端默认为 `https://app.memorylake.cn/openapi/memorylake`,无需配置。

### 3) 对 project 启用

```bash
engram memorylake enable --project <project名>
```

首次 enable 会自动把该 project 在本地 SQLite 中的已有记忆迁移到 MemoryLake(幂等,可重复执行)。enable / disable 对**正在运行的 agent session 也即时生效**,无需重启。

### 4) 查看 / 关闭

```bash
engram memorylake status                          # 查看各 project 当前后端
engram memorylake disable --project <project名>   # 改回本地 SQLite
```

## 参考

完整安装方式(手动安装、源码编译、Windows)、升级说明、API / CLI / 环境变量:见仓库根 `DOCS.md` 与 [Releases](https://github.com/relytcloud/engram/releases)。
