package builtins

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"mcp-ops/internal/mcp"
)

// RegisterGit 注册官方 server-git 的工具集（基于系统 git 命令，参数数组传递不经过 shell）。
func RegisterGit(s *mcp.Server, defaultRepo string) {
	g := &gitOps{defaultRepo: defaultRepo}

	s.RegisterTool("git_status",
		"查看工作区状态（已暂存/未暂存/未跟踪文件）。",
		schema(`"repo_path":{"type":"string","description":"仓库路径，可省略用默认仓库"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return g.run(argString(args, "repo_path"), "status", "--porcelain=v1", "-b")
		})

	s.RegisterTool("git_diff_unstaged",
		"查看未暂存的改动 diff。",
		schema(`"repo_path":{"type":"string"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return g.run(argString(args, "repo_path"), "diff")
		})

	s.RegisterTool("git_diff_staged",
		"查看已暂存的改动 diff。",
		schema(`"repo_path":{"type":"string"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return g.run(argString(args, "repo_path"), "diff", "--staged")
		})

	s.RegisterTool("git_diff",
		"查看与指定目标（分支/提交）之间的差异。",
		schema(`"repo_path":{"type":"string"},"target":{"type":"string","description":"对比目标，如 HEAD~1 或分支名"}`, "target"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			target := argString(args, "target")
			if target == "" {
				target = "HEAD"
			}
			return g.run(argString(args, "repo_path"), "diff", target)
		})

	s.RegisterTool("git_commit",
		"提交已暂存的改动。",
		schema(`"repo_path":{"type":"string"},"message":{"type":"string"}`, "message"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			msg := argString(args, "message")
			if msg == "" {
				return errR("message 不能为空")
			}
			return g.run(argString(args, "repo_path"), "commit", "-m", msg)
		})

	s.RegisterTool("git_add",
		"暂存文件（files 为空则 add -A 全部）。",
		schema(`"repo_path":{"type":"string"},"files":{"type":"array","items":{"type":"string"}}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			files := argStrings(args, "files")
			if len(files) == 0 {
				return g.run(argString(args, "repo_path"), "add", "-A")
			}
			return g.run(argString(args, "repo_path"), append([]string{"add"}, files...)...)
		})

	s.RegisterTool("git_reset",
		"取消暂存（--mixed，保留工作区改动）。",
		schema(`"repo_path":{"type":"string"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return g.run(argString(args, "repo_path"), "reset")
		})

	s.RegisterTool("git_log",
		"查看提交历史。",
		schema(`"repo_path":{"type":"string"},"max_count":{"type":"integer","description":"最多返回条数，默认 10"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			n := argInt(args, "max_count")
			if n <= 0 {
				n = 10
			}
			return g.run(argString(args, "repo_path"), "log", "--oneline", "-n", strconv.Itoa(n))
		})

	s.RegisterTool("git_create_branch",
		"创建新分支（可选指定起始点）。",
		schema(`"repo_path":{"type":"string"},"branch_name":{"type":"string"},"start_point":{"type":"string"}`, "branch_name"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			name := argString(args, "branch_name")
			if name == "" {
				return errR("branch_name 不能为空")
			}
			cmd := []string{"branch", name}
			if sp := argString(args, "start_point"); sp != "" {
				cmd = append(cmd, sp)
			}
			return g.run(argString(args, "repo_path"), cmd...)
		})

	s.RegisterTool("git_checkout",
		"切换到指定分支或提交。",
		schema(`"repo_path":{"type":"string"},"branch_name":{"type":"string"}`, "branch_name"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			name := argString(args, "branch_name")
			if name == "" {
				return errR("branch_name 不能为空")
			}
			return g.run(argString(args, "repo_path"), "checkout", name)
		})

	s.RegisterTool("git_show",
		"查看某次提交的详细信息。",
		schema(`"repo_path":{"type":"string"},"revision":{"type":"string","description":"提交哈希或引用，默认 HEAD"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			rev := argString(args, "revision")
			if rev == "" {
				rev = "HEAD"
			}
			return g.run(argString(args, "repo_path"), "show", "--stat", "--patch", rev)
		})

	s.RegisterTool("git_branch",
		"列出本地（或含远程）分支。",
		schema(`"repo_path":{"type":"string"},"show_all":{"type":"boolean"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if argBool(args, "show_all") {
				return g.run(argString(args, "repo_path"), "branch", "-a")
			}
			return g.run(argString(args, "repo_path"), "branch")
		})
}

type gitOps struct {
	defaultRepo string
}

func (g *gitOps) run(repo string, args ...string) *mcp.CallToolResult {
	if repo == "" {
		repo = g.defaultRepo
	}
	if repo == "" {
		return errR("未指定 repo_path 且未配置 builtins.git.default_repo")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return errR("仓库路径无效: %v", err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = abs
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return errR("git %s 失败: %v", args[0], err)
		}
		return errR("git %s 失败: %v\n%s", args[0], err, text)
	}
	if text == "" {
		text = "(无输出)"
	}
	return mcp.TextResult(text)
}

// ensureGit 供测试/文档说明使用：确认系统 git 可用。
func ensureGit() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("未找到 git 命令: %w", err)
	}
	if _, err := os.Stat("/tmp"); err != nil {
		return err
	}
	return nil
}
