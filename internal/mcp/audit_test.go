package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestAuditToolCall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("ENGRAM_MCP_AUDIT_LOG", p)
	auditToolCall("mem_save")
	auditToolCall("mem_search")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " mem_save") || !strings.HasSuffix(lines[1], " mem_search") {
		t.Fatalf("unexpected audit content: %q", string(b))
	}
}

func TestAuditToolCallDisabled(t *testing.T) {
	t.Setenv("ENGRAM_MCP_AUDIT_LOG", "")
	auditToolCall("mem_save") // must not panic or create files
}

// withAudit must be a transparent wrapper: it records the call arrival and then
// returns the inner handler's (result, error) pair completely unchanged.
func TestWithAuditPassesResultAndErrorThrough(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("ENGRAM_MCP_AUDIT_LOG", p)

	sentinelRes := mcppkg.NewToolResultText("sentinel-payload")
	sentinelErr := errors.New("sentinel-error")
	calls := 0
	stub := func(ctx context.Context, req mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		calls++
		return sentinelRes, sentinelErr
	}

	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"k": "v"}}}
	res, err := withAudit("x", stub)(context.Background(), req)

	if calls != 1 {
		t.Fatalf("inner handler called %d times, want 1", calls)
	}
	if res != sentinelRes {
		t.Errorf("result not passed through: got %+v, want %+v", res, sentinelRes)
	}
	if !errors.Is(err, sentinelErr) {
		t.Errorf("error not passed through: got %v, want %v", err, sentinelErr)
	}

	b, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatalf("audit file: %v", readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], " x") {
		t.Fatalf("expected one audit line for %q, got %q", "x", string(b))
	}
}
