// mcp-ops：自研 MCP 客户端（host）+ SSH MCP Server + 数据库 MCP Server，
// 单二进制多子命令交付。
//
//	opsctl host        # 启动交互式 Agent 客户端（连接 LLM 与两个 MCP Server）
//	opsctl ssh-server  # 启动 SSH MCP Server（Streamable HTTP）
//	opsctl db-server   # 启动数据库 MCP Server（Streamable HTTP）
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"mcp-ops/internal/builtins"
	"mcp-ops/internal/config"
	"mcp-ops/internal/dbsrv"
	"mcp-ops/internal/host"
	"mcp-ops/internal/llm"
	"mcp-ops/internal/mcp"
	"mcp-ops/internal/postmansrv"
	"mcp-ops/internal/sshsrv"
	"mcp-ops/internal/websrv"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入（见 scripts/release.sh）
var version = "0.1.0-dev"

func main() {
	cfgPath := "config.yaml"
	args := os.Args[1:]

	// 解析 -config 全局参数
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-config" || args[i] == "--config" {
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		} else {
			rest = append(rest, args[i])
		}
	}
	cmd := ""
	if len(rest) > 0 {
		cmd = rest[0]
	}

	switch cmd {
	case "host":
		runHost(cfgPath)
	case "ssh-server":
		runSSHServer(cfgPath)
	case "db-server":
		runDBServer(cfgPath)
	case "web-server":
		runWebServer(cfgPath)
	case "stdio":
		runStdio(cfgPath)
	case "version", "--version":
		fmt.Println("mcp-ops", version)
	case "help", "--help", "-h", "":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`mcp-ops ` + version + ` — 自研通用 MCP 客户端 + SSH/数据库/网页 MCP Server

用法: opsctl <command> [-config <path>]

命令:
  host        通用 MCP 客户端：内置 SSH/DB/Web 工具 + 可挂载外部 stdio server
              （npx 官方 server、playwright-mcp、chrome-devtools-mcp 等，见 mcp_servers 段）
  ssh-server  启动 SSH MCP Server (Streamable HTTP + Bearer token)
  db-server   启动数据库 MCP Server (MySQL/PostgreSQL/MongoDB/Redis/ES)
  web-server  启动网页抓取 MCP Server (fetch_url：正文提取 + robots + SSRF 防护)
  stdio       以 stdio 传输运行全部内置工具（供 Kiro/Claude Desktop/Cursor 拉起）
  version     显示版本

配置文件默认为 ./config.yaml，热重载（SIGHUP 或修改文件后 2s 内生效）。
`)
}

func loadOrDie(path string) *config.Manager {
	m, err := config.LoadAndWatch(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	return m
}

func runSSHServer(cfgPath string) {
	mgr := loadOrDie(cfgPath)
	cfg := mgr.Get()
	pool := sshsrv.NewPool(mgr)
	defer pool.CloseAll()

	srv := mcp.NewServer(mcp.Implementation{Name: "opsctl-ssh", Version: version}, cfg.SSHServer.Token)
	sshsrv.RegisterTools(srv, pool)

	addr := cfg.SSHServer.Listen
	fmt.Printf("SSH MCP Server listening on http://%s/mcp (hosts: %d)\n", addr, len(cfg.SSHHosts))
	if err := http.ListenAndServe(addr, srv.Handler("/mcp")); err != nil {
		fmt.Fprintf(os.Stderr, "ssh-server: %v\n", err)
		os.Exit(1)
	}
}

func runDBServer(cfgPath string) {
	mgr := loadOrDie(cfgPath)
	cfg := mgr.Get()

	pool := sshsrv.NewPool(mgr) // 供数据库 via_ssh 隧道复用
	defer pool.CloseAll()
	dbMgr := dbsrv.NewManager(mgr, pool)

	srv := mcp.NewServer(mcp.Implementation{Name: "opsctl-db", Version: version}, cfg.DBServer.Token)
	dbsrv.RegisterTools(srv, dbMgr)

	addr := cfg.DBServer.Listen
	fmt.Printf("DB MCP Server listening on http://%s/mcp (databases: %d)\n", addr, len(cfg.Databases))
	if err := http.ListenAndServe(addr, srv.Handler("/mcp")); err != nil {
		fmt.Fprintf(os.Stderr, "db-server: %v\n", err)
		os.Exit(1)
	}
}

// runWebServer 启动网页抓取 MCP Server。
// 与 SSH/DB Server 一样走 Streamable HTTP + Bearer token，
// 抓取策略（robots、SSRF、大小、缓存）全部来自 web_fetch 段。
func runWebServer(cfgPath string) {
	mgr := loadOrDie(cfgPath)
	cfg := mgr.Get()

	fetcher := websrv.NewFetcher(mgr)
	srv := mcp.NewServer(mcp.Implementation{Name: "opsctl-web", Version: version}, cfg.WebServer.Token)
	websrv.RegisterTools(srv, fetcher)

	addr := cfg.WebServer.Listen
	if addr == "" {
		addr = "127.0.0.1:8803"
	}
	fmt.Printf("Web MCP Server listening on http://%s/mcp (robots=%v, allow_private=%v, max_bytes=%d)\n",
		addr, cfg.WebFetch.Robots(), cfg.WebFetch.AllowPrivate, cfg.WebFetch.Bytes())
	if err := http.ListenAndServe(addr, srv.Handler("/mcp")); err != nil {
		fmt.Fprintf(os.Stderr, "web-server: %v\n", err)
		os.Exit(1)
	}
}

// runStdio 以 stdio 传输运行：一个进程内同时暴露 SSH 与数据库全部工具。
// 日志必须走 stderr（stdout 只允许 JSON-RPC），否则会破坏协议流。
func runStdio(cfgPath string) {
	mgr, err := config.LoadAndWatch(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	pool := sshsrv.NewPool(mgr)
	defer pool.CloseAll()
	dbMgr := dbsrv.NewManager(mgr, pool)

	srv := mcp.NewServer(mcp.Implementation{Name: "opsctl", Version: version}, "")
	sshsrv.RegisterTools(srv, pool)
	dbsrv.RegisterTools(srv, dbMgr)
	websrv.RegisterTools(srv, websrv.NewFetcher(mgr))
	postmansrv.RegisterTools(srv, mgr)
	builtins.RegisterAll(srv, mgr, websrv.NewFetcher(mgr))

	fmt.Fprintf(os.Stderr, "[opsctl stdio] ready: %d ssh hosts, %d databases, web fetch enabled, config=%s\n",
		len(mgr.Get().SSHHosts), len(mgr.Get().Databases), cfgPath)
	if err := srv.ServeStdio(); err != nil {
		fmt.Fprintf(os.Stderr, "[opsctl stdio] %v\n", err)
		os.Exit(1)
	}
}

func runHost(cfgPath string) {
	mgr := loadOrDie(cfgPath)
	cfg := mgr.Get()

	// 构造 LLM Provider（配置热重载后自动生效）
	provider := buildProvider(mgr)

	agent := host.NewAgent(provider,
		func() string { return mgr.Get().LLM.SystemPrompt },
		func() int { return mgr.Get().LLM.MaxTurns },
	)
	defer agent.Close()

	// 内置能力：SSH + DB + Web 在进程内注册，无需再单独起 HTTP server
	pool := sshsrv.NewPool(mgr)
	defer pool.CloseAll()
	dbMgr := dbsrv.NewManager(mgr, pool)
	fetcher := websrv.NewFetcher(mgr)

	builtin := mcp.NewServer(mcp.Implementation{Name: "opsctl-builtin", Version: version}, "")
	sshsrv.RegisterTools(builtin, pool)
	dbsrv.RegisterTools(builtin, dbMgr)
	websrv.RegisterTools(builtin, fetcher)
	postmansrv.RegisterTools(builtin, mgr)
	builtins.RegisterAll(builtin, mgr, fetcher)
	if err := agent.RegisterServer(mcp.NewInProcServer("builtin", builtin)); err != nil {
		fmt.Fprintf(os.Stderr, "内置工具注册失败: %v\n", err)
	}

	// 外部 stdio MCP server：npx 官方 server、playwright-mcp、chrome-devtools-mcp 等
	external := 0
	for _, ms := range cfg.MCPServers {
		if ms.Disabled {
			continue
		}
		c := mcp.NewStdioClient(ms.Name, ms.Command, ms.Args, ms.Env)
		if err := agent.RegisterServer(c); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  加载外部 MCP server %q 失败（已跳过）: %v\n", ms.Name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "✅ 已加载外部 MCP server %q\n", ms.Name)
		external++
	}

	banner := fmt.Sprintf("mcp-ops host %s — provider=%s, tools=%d（内置 + %d 个外部 server）",
		version, provider.Name(), len(agent.Tools()), external)
	host.RunREPL(agent, banner)
}

// buildProvider 每次都读最新配置构造 provider（简单起见每次 Chat 前重建；
// 这里在启动时构造一次，热重载后的 provider 切换留作后续优化）
func buildProvider(mgr *config.Manager) llm.Provider {
	cfg := mgr.Get()
	switch strings.ToLower(cfg.LLM.Provider) {
	case "anthropic":
		return llm.NewAnthropic(&llm.AnthropicConfig{
			BaseURL: cfg.LLM.Anthropic.BaseURL,
			APIKey:  cfg.LLM.Anthropic.APIKey,
			Model:   cfg.LLM.Anthropic.Model,
		})
	default:
		return llm.NewOpenAI(&llm.OpenAIConfig{
			BaseURL: cfg.LLM.OpenAI.BaseURL,
			APIKey:  cfg.LLM.OpenAI.APIKey,
			Model:   cfg.LLM.OpenAI.Model,
		})
	}
}
