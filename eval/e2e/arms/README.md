# L3 eval arms

Each arm is a **template** for a fresh `CLAUDE_CONFIG_DIR`. The L3 runner
(`eval/cmd/evalrun`, suite `l3`) materializes an arm per run into a temp
workdir via `e2e.MaterializeArm(templateDir, workDir, engramBin)`, which
copies the template tree verbatim and substitutes the literal string
`{{ENGRAM_BIN}}` inside every `.json` file with the arm's engram binary path.

## Isolation model

Each arm gets an isolated config dir so nothing from the user's real
`~/.claude` leaks into a run. Claude is always invoked with
`--strict-mcp-config`, so only the MCP servers this arm declares are loaded.

### `no-memory/`
Control arm. No plugins, no hooks, no MCP servers — `settings.json` has an
empty `hooks` object and there is no `mcp.json`. With `--strict-mcp-config`
and no `--mcp-config`, Claude runs with zero memory tooling. The isolation
probe asserts **no** `mem_*` tools are present.

### `memory/`
Treatment arm. `mcp.json` registers engram as a stdio MCP server
(`{{ENGRAM_BIN}} mcp`), and `settings.json` injects the memory protocol via a
`SessionStart` hook that runs `plugin/claude-code/scripts/session-start.sh`.
The isolation probe asserts `mem_*` tools **are** present.

## Baseline vs. optimized

There is only one `memory` template. The **baseline** and **optimized** arms
are both this template materialized with a *different* `engramBin` — the
binary is the only thing that differs between them. This keeps the config,
hook, and MCP wiring identical so any measured uplift is attributable to the
engram binary's behavior, not to config drift.
