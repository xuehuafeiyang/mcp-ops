package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-ops/internal/llm"
	"mcp-ops/internal/mcp"
)

// echoServerPy 是一个 stdio MCP 假 server，argv[1] 作为前缀，
// 暴露一个 "echo" 工具，返回 "<prefix>:<arguments>"。
const echoServerPy = `
import sys, json
prefix = sys.argv[1] if len(sys.argv) > 1 else "X"
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
        print(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result":
            {"protocolVersion": "2025-03-26", "capabilities": {},
             "serverInfo": {"name": prefix, "version": "1"}}}), flush=True)
    elif m == "tools/list":
        print(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result":
            {"tools": [{"name": "echo", "description": "echo",
                        "inputSchema": {"type": "object"}}]}}), flush=True)
    elif m == "tools/call":
        args = req.get("params", {}).get("arguments")
        print(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result":
            {"content": [{"type": "text", "text": prefix + ":" + json.dumps(args)}],
             "isError": False}}), flush=True)
`

func echoServer(t *testing.T, prefix string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "echo_server.py")
	if err := os.WriteFile(p, []byte(echoServerPy), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeProvider 第一轮返回一个工具调用，之后返回纯文本。
type fakeProvider struct {
	calls    int
	toolName string
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Chat(_ context.Context, _ string, _ []llm.Message, _ []mcp.Tool) (*llm.Message, error) {
	f.calls++
	if f.calls == 1 {
		return &llm.Message{Role: "assistant", Text: "调用工具",
			ToolCalls: []llm.ToolCall{{ID: "1", Name: f.toolName, Arguments: `{"x":1}`}}}, nil
	}
	return &llm.Message{Role: "assistant", Text: "done"}, nil
}

func TestAgentMergesRenamedToolsAndRoutesCorrectly(t *testing.T) {
	script := echoServer(t, "X")

	a := NewAgent(&fakeProvider{}, func() string { return "" }, func() int { return 5 })
	defer a.Close()

	if err := a.RegisterServer(mcp.NewStdioClient("a", "python3", []string{script, "AAA"}, nil)); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := a.RegisterServer(mcp.NewStdioClient("b", "python3", []string{script, "BBB"}, nil)); err != nil {
		t.Fatalf("register b: %v", err)
	}

	// 两个 server 都有 "echo"，第二个应被改名为 "b__echo"
	names := map[string]bool{}
	for _, tool := range a.Tools() {
		names[tool.Name] = true
	}
	if !names["echo"] || !names["b__echo"] {
		t.Fatalf("expected echo 与 b__echo 都在，实际: %v", names)
	}

	// 调用 b__echo 必须路由到 server b 并用原始名 echo 调用，返回 "BBB:..."
	sess := NewSession(a)
	result := sess.callTool(llm.ToolCall{ID: "1", Name: "b__echo", Arguments: `{"x":1}`}, nil)
	if !strings.Contains(result, "BBB:") {
		t.Fatalf("b__echo 应返回 BBB 前缀，实际: %q", result)
	}
	if strings.Contains(result, "AAA:") {
		t.Fatalf("b__echo 错误路由到了 server a: %q", result)
	}
}

func TestAgentTurnEndToEnd(t *testing.T) {
	script := echoServer(t, "X")
	prov := &fakeProvider{toolName: "echo"}

	a := NewAgent(prov, func() string { return "sys" }, func() int { return 5 })
	defer a.Close()

	if err := a.RegisterServer(mcp.NewStdioClient("only", "python3", []string{script, "OK"}, nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	sess := NewSession(a)
	text, err := sess.Turn("hi", nil)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want done", text)
	}
	// 工具结果应已回传进历史
	found := false
	for _, m := range sess.messages {
		if m.Role == "tool" && strings.Contains(m.ToolResult, "OK:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("工具结果未回传: %+v", sess.messages)
	}
}
