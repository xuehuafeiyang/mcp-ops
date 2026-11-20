package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServerPy 是一个极简的 stdio MCP 假 server：
// 处理 initialize / tools/list / tools/call / ping，验证客户端协议是否正确。
const fakeServerPy = `
import sys, json, os

def respond(i, result):
    print(json.dumps({"jsonrpc": "2.0", "id": i, "result": result}), flush=True)

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    m = req.get("method")
    if m == "initialize":
        respond(req["id"], {"protocolVersion": "2025-03-26", "capabilities": {},
                            "serverInfo": {"name": "fake", "version": "1.0"}})
    elif m == "tools/list":
        respond(req["id"], {"tools": [
            {"name": "echo", "description": "echo arguments back",
             "inputSchema": {"type": "object", "properties": {}}},
            {"name": "boom", "description": "exits the server",
             "inputSchema": {"type": "object", "properties": {}}},
        ]})
    elif m == "tools/call":
        p = req.get("params", {})
        if p.get("name") == "boom":
            os._exit(0)  # 模拟进程在调用中途崩溃
        respond(req["id"], {"content": [{"type": "text", "text": "echo:" + json.dumps(p.get("arguments"))}],
                            "isError": False})
    elif m == "ping":
        respond(req["id"], {})
`

func fakeServer(t *testing.T) (scriptPath string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available, skipping stdio client integration test")
	}
	dir := t.TempDir()
	scriptPath = filepath.Join(dir, "fake_server.py")
	if err := os.WriteFile(scriptPath, []byte(fakeServerPy), 0o700); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

func TestStdioClientLifecycle(t *testing.T) {
	script := fakeServer(t)
	c := NewStdioClient("fake", "python3", []string{script}, nil)
	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer c.Close()

	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	res, err := c.CallTool("echo", map[string]any{"x": 1, "s": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := res.ResultText()
	if !strings.Contains(got, "echo:") || !strings.Contains(got, `"x": 1`) {
		t.Fatalf("unexpected result text: %q", got)
	}
	if res.IsError {
		t.Fatalf("expected non-error result")
	}
}

func TestStdioClientStartFailure(t *testing.T) {
	c := NewStdioClient("bad", "definitely-not-a-real-command-xyz", nil, nil)
	err := c.Initialize()
	if err == nil {
		t.Fatal("expected start failure, got nil")
	}
	if !strings.Contains(err.Error(), "启动失败") {
		t.Fatalf("expected 启动失败 in error, got: %v", err)
	}
}

func TestStdioClientExitDuringCall(t *testing.T) {
	script := fakeServer(t)
	c := NewStdioClient("fake", "python3", []string{script}, nil)
	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer c.Close()

	// boom 工具会让假 server 直接退出，客户端应返回错误而非挂起
	_, err := c.CallTool("boom", map[string]any{})
	if err == nil {
		t.Fatal("expected error when server exits mid-call, got nil")
	}
}
