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

首次 enable 会自动把该 project 在本地 SQLite 中的已有记忆迁移到 MemoryLake(幂等,可重复执行)。

### 4) 查看 / 关闭

```bash
engram memorylake status                          # 查看各 project 当前后端
engram memorylake disable --project <project名>   # 改回本地 SQLite
```

## 参考

完整安装方式(手动安装、源码编译、Windows)、升级说明、API / CLI / 环境变量:见仓库根 `DOCS.md` 与 [Releases](https://github.com/relytcloud/engram/releases)。
