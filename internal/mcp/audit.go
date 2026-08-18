package mcp

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// auditToolCall appends one "<RFC3339> <tool>" line to the file named by
// ENGRAM_MCP_AUDIT_LOG. Opt-in, best-effort: any failure is ignored so the
// audit can never break a tool call. Used by the eval harness to count
// mem_* usage per session (Phase 2 spec: R2 save-count monitoring).
func auditToolCall(tool string) {
	path := os.Getenv("ENGRAM_MCP_AUDIT_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), tool)
}

// withAudit wraps a tool handler so every invocation is recorded by
// auditToolCall before the real handler runs. It is applied at every
// mem_* AddTool site in registerTools.
func withAudit(name string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		auditToolCall(name)
		return h(ctx, req)
	}
}
