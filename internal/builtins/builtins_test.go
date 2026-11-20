package builtins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-ops/internal/config"
	"mcp-ops/internal/mcp"
	"mcp-ops/internal/websrv"
)

func testServer(t *testing.T, dirs []string) *mcp.InProcServer {
	t.Helper()
	cfg := &config.Config{}
	cfg.Builtins.Filesystem.AllowedDirectories = dirs
	cfg.Builtins.Memory.StoragePath = filepath.Join(t.TempDir(), "memory.json")
	mgr := config.NewManager("", cfg)

	srv := mcp.NewServer(mcp.Implementation{Name: "test-builtins", Version: "0"}, "")
	RegisterAll(srv, mgr, websrv.NewFetcher(mgr))
	return mcp.NewInProcServer("builtins", srv)
}

func call(t *testing.T, s *mcp.InProcServer, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestFilesystemRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := testServer(t, []string{dir})

	// 写文件
	res := call(t, s, "write_file", map[string]any{"path": filepath.Join(dir, "a.txt"), "content": "hello\nworld"})
	if res.IsError {
		t.Fatalf("write_file: %s", res.ResultText())
	}

	// 读文件（带行号）
	res = call(t, s, "read_file", map[string]any{"path": filepath.Join(dir, "a.txt")})
	if res.IsError || !strings.Contains(res.ResultText(), "hello") {
		t.Fatalf("read_file: %q err=%v", res.ResultText(), res.IsError)
	}

	// 目录外路径必须被拒绝
	res = call(t, s, "read_file", map[string]any{"path": "/etc/passwd"})
	if !res.IsError || !strings.Contains(res.ResultText(), "不在允许的目录内") {
		t.Fatalf("路径穿越未被拦截: %q err=%v", res.ResultText(), res.IsError)
	}

	// 列表目录
	res = call(t, s, "list_directory", map[string]any{"path": dir})
	if res.IsError {
		t.Fatalf("list_directory: %s", res.ResultText())
	}
}

func TestFilesystemEditAndSearch(t *testing.T) {
	dir := t.TempDir()
	s := testServer(t, []string{dir})

	p := filepath.Join(dir, "cfg.txt")
	call(t, s, "write_file", map[string]any{"path": p, "content": "port = 8080\n"})

	// dry run 预览
	res := call(t, s, "edit_file", map[string]any{
		"path":   p,
		"edits":  []map[string]any{{"oldText": "8080", "newText": "9090"}},
		"dryRun": true,
	})
	if res.IsError || !strings.Contains(res.ResultText(), "9090") {
		t.Fatalf("edit_file dryRun: %q", res.ResultText())
	}
	// 落盘
	call(t, s, "edit_file", map[string]any{
		"path":  p,
		"edits": []map[string]any{{"oldText": "8080", "newText": "9090"}},
	})
	res = call(t, s, "read_file", map[string]any{"path": p})
	if !strings.Contains(res.ResultText(), "9090") {
		t.Fatalf("edit 未落盘: %q", res.ResultText())
	}

	// 搜索
	res = call(t, s, "search_files", map[string]any{"path": dir, "pattern": "*.txt"})
	if res.IsError || !strings.Contains(res.ResultText(), "cfg.txt") {
		t.Fatalf("search_files: %q", res.ResultText())
	}
}

func TestMemoryGraph(t *testing.T) {
	dir := t.TempDir()
	s := testServer(t, []string{dir})

	res := call(t, s, "create_entities", map[string]any{
		"entities": []map[string]any{
			{"name": "Alice", "entityType": "person", "observations": []string{"工程师"}},
			{"name": "Bob", "entityType": "person"},
		},
	})
	if res.IsError {
		t.Fatalf("create_entities: %s", res.ResultText())
	}

	res = call(t, s, "create_relations", map[string]any{
		"relations": []map[string]any{{"from": "Alice", "to": "Bob", "relationType": "同事"}},
	})
	if res.IsError {
		t.Fatalf("create_relations: %s", res.ResultText())
	}

	res = call(t, s, "read_graph", map[string]any{})
	if res.IsError || !strings.Contains(res.ResultText(), "Alice") || !strings.Contains(res.ResultText(), "同事") {
		t.Fatalf("read_graph: %q", res.ResultText())
	}

	res = call(t, s, "search_nodes", map[string]any{"query": "Alice"})
	if res.IsError || !strings.Contains(res.ResultText(), "Alice") {
		t.Fatalf("search_nodes: %q", res.ResultText())
	}
}

func TestSequentialThinking(t *testing.T) {
	s := testServer(t, []string{t.TempDir()})
	res := call(t, s, "sequentialthinking", map[string]any{
		"thought":           "第一步：明确目标",
		"thoughtNumber":     1,
		"totalThoughts":     2,
		"nextThoughtNeeded": true,
	})
	if res.IsError || !strings.Contains(res.ResultText(), "步骤 1") {
		t.Fatalf("sequentialthinking: %q err=%v", res.ResultText(), res.IsError)
	}
}

func TestEverything(t *testing.T) {
	s := testServer(t, []string{t.TempDir()})

	if res := call(t, s, "echo", map[string]any{"message": "ping"}); res.ResultText() != "ping" {
		t.Fatalf("echo: %q", res.ResultText())
	}
	if res := call(t, s, "add", map[string]any{"a": 2.0, "b": 3.0}); strings.TrimSpace(res.ResultText()) != "5" {
		t.Fatalf("add: %q", res.ResultText())
	}
}

func TestGitTools(t *testing.T) {
	// 当前仓库（mcp-ops 本身是 git 仓库）
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Skipf("无法定位仓库: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Skipf("%s 不是 git 仓库: %v", repo, err)
	}
	s := testServer(t, []string{repo})

	res := call(t, s, "git_status", map[string]any{"repo_path": repo})
	if res.IsError {
		t.Fatalf("git_status: %s", res.ResultText())
	}
	res = call(t, s, "git_branch", map[string]any{"repo_path": repo})
	if res.IsError {
		t.Fatalf("git_branch: %s", res.ResultText())
	}
}

func TestToolCount(t *testing.T) {
	s := testServer(t, []string{t.TempDir()})
	tools, err := s.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	// 7 个内置 server 应贡献大量工具（filesystem 11 + memory 9 + seq 1 + everything 8 + fetch 1 + git 12 + playwright 15 = 57）
	if len(tools) < 50 {
		t.Fatalf("内置工具数量异常：%d（预期 ≥50）", len(tools))
	}
	t.Logf("内置工具共 %d 个", len(tools))
}
