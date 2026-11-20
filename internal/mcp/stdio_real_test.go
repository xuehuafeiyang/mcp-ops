package mcp

import (
	"os"
	"testing"
)

// TestRealFilesystemServer 是可选的真实集成测试：
// 通过 npx 拉起官方 @modelcontextprotocol/server-filesystem，验证真实调用。
//
// 默认跳过（依赖 npx 网络）；运行方式：
//
//	REAL_MCP_TEST=1 go test ./internal/mcp/ -run TestRealFilesystemServer -v
func TestRealFilesystemServer(t *testing.T) {
	if os.Getenv("REAL_MCP_TEST") == "" {
		t.Skip("set REAL_MCP_TEST=1 to run against real npx servers")
	}
	c := NewStdioClient("filesystem", "npx",
		[]string{"-y", "@modelcontextprotocol/server-filesystem", "/workspace"}, nil)
	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer c.Close()

	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	t.Logf("filesystem exposes %d tools", len(tools))

	res, err := c.CallTool("read_file", map[string]any{"path": "/workspace/mcp-ops/go.mod"})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_file returned error: %s", res.ResultText())
	}
	text := res.ResultText()
	if len(text) == 0 || text[0] == '[' {
		t.Fatalf("unexpected read_file output: %.120q", text)
	}
	t.Logf("read_file OK, first line: %.80s", text)
}
