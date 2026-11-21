// Package builtins 以纯 Go 原生实现 8 个官方/社区 MCP server 的核心能力，
// 作为 opsctl 单二进制的内置工具，替代通过 npx / 全局命令拉起外部进程的方式。
//
//  1. filesystem          —— 本地文件读写搜索（限定目录）
//  2. memory              —— 持久化知识图谱记忆
//  3. sequentialthinking  —— 结构化逐步推理
//  4. everything          —— 协议测试工具子集
//  5. fetch               —— 网页抓取转 Markdown（复用 websrv）
//  6. git                 —— 本地 Git 仓库操作
//  7. playwright          —— 浏览器自动化（chromedp/CDP，含网络请求监控）
package builtins

import (
	"mcp-ops/internal/config"
	"mcp-ops/internal/mcp"
	"mcp-ops/internal/websrv"
)

// RegisterAll 把 7 个内置 server 的工具一次性注册到 s 上。
// fetcher 由调用方传入（复用同一份抓取配置与缓存）。
func RegisterAll(s *mcp.Server, mgr *config.Manager, fetcher *websrv.Fetcher) {
	cfg := mgr.Get()
	RegisterFilesystem(s, cfg.Builtins.Filesystem.Dirs())
	RegisterSequentialThinking(s)
	RegisterMemory(s, cfg.Builtins.Memory.Store())
	RegisterGit(s, cfg.Builtins.Git.Repo())
	RegisterFetch(s, fetcher)
	RegisterEverything(s)
	RegisterBrowserTools(s, cfg.Builtins.Browser)
}
