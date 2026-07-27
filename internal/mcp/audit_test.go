package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
