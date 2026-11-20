package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"mcp-ops/internal/llm"
	"mcp-ops/internal/mcp"
)

// defaultSystemPrompt 是配置中 system_prompt 留空时使用的内置运维提示词。
const defaultSystemPrompt = `你是一个运维助手，可以调用 MCP 工具在多台 SSH 服务器上执行命令、在多个数据库上查询与写入。

工作准则：
1. 不确定主机名或库名时，先调用 list_ssh_hosts / list_databases 查看，不要猜测。
2. 一次只做用户要求的事；涉及写入、删除、重启等有副作用的操作，先说明将要执行什么再执行。
3. 工具返回 POLICY DENIED 时不要尝试绕过（改写命令、拆分拼接都不允许），直接向用户说明被策略拒绝及原因。
4. 返回 TIMEOUT 时缩小范围重试（加 LIMIT、缩小时间区间、减少输出），不要原样重发。
5. 输出给用户前做归纳：命令的原始输出不必全文复述，给结论、关键数字和异常项。`

// Agent 是核心 Agent 循环：LLM ↔ MCP 工具，直到模型给出最终回答。
type Agent struct {
	provider llm.Provider
	system   func() string
	maxTurns func() int

	mu        sync.RWMutex
	clients   map[string]mcp.ToolServer
	routes    map[string]string // 工具名 → server 名（冲突时自动加 server 前缀）
	origNames map[string]string // 最终工具名 → server 端原始工具名（改名后调用要用原名）
	tools     []mcp.Tool
}

// NewAgent 构造并从各 MCP Server 拉取工具清单
func NewAgent(provider llm.Provider, system func() string, maxTurns func() int) *Agent {
	return &Agent{
		provider: provider, system: system, maxTurns: maxTurns,
		clients: map[string]mcp.ToolServer{}, routes: map[string]string{},
		origNames: map[string]string{},
	}
}

// RegisterServer 注册一个工具来源（HTTP / stdio 子进程 / 进程内），
// 握手并合并其工具。工具名冲突（两个 server 重名工具）时自动改名为 "server__tool"。
func (a *Agent) RegisterServer(ts mcp.ToolServer) error {
	if err := ts.Initialize(); err != nil {
		return err
	}
	tools, err := ts.ListTools()
	if err != nil {
		_ = ts.Close()
		return err
	}
	name := ts.Name()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.clients[name] = ts
	for _, t := range tools {
		finalName := t.Name
		if _, exists := a.routes[t.Name]; exists {
			finalName = name + "__" + t.Name
		}
		a.routes[finalName] = name
		a.origNames[finalName] = t.Name // 记录原始名，调用时翻译回去
		nt := t
		nt.Name = finalName
		a.tools = append(a.tools, nt)
	}
	return nil
}

// RegisterHTTPServer 便捷方法：连接一个远程 Streamable HTTP MCP server。
func (a *Agent) RegisterHTTPServer(name, url, token string) error {
	return a.RegisterServer(mcp.NewClient(name, url, token))
}

// Close 关闭所有已注册的工具来源（终止 stdio 子进程等）。
func (a *Agent) Close() {
	a.mu.Lock()
	clients := make([]mcp.ToolServer, 0, len(a.clients))
	for _, c := range a.clients {
		clients = append(clients, c)
	}
	a.clients = map[string]mcp.ToolServer{}
	a.routes = map[string]string{}
	a.origNames = map[string]string{}
	a.tools = nil
	a.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}
}

// Tools 返回合并后的工具清单快照
func (a *Agent) Tools() []mcp.Tool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]mcp.Tool{}, a.tools...)
}

// Session 一次 REPL 会话（含消息历史）
type Session struct {
	agent    *Agent
	messages []llm.Message
}

func NewSession(agent *Agent) *Session { return &Session{agent: agent} }

// Reset 清空会话历史
func (s *Session) Reset() { s.messages = nil }

// HistoryLen 历史消息条数
func (s *Session) HistoryLen() int { return len(s.messages) }

// Turn 执行一轮用户输入：跑完整 Agent 循环并返回最终文本。
// progress 回调用于 REPL 展示中间步骤（可为 nil）。
func (s *Session) Turn(userInput string, progress func(line string)) (string, error) {
	a := s.agent
	s.messages = append(s.messages, llm.Message{Role: "user", Text: userInput})

	maxTurns := a.maxTurns()
	if maxTurns <= 0 {
		maxTurns = 30 // 配置省略时的兜底，防止循环一次都不执行
	}
	sys := a.system()
	if strings.TrimSpace(sys) == "" {
		sys = defaultSystemPrompt
	}
	for turn := 0; turn < maxTurns; turn++ {
		am, err := a.provider.Chat(ctxBG(), sys, s.messages, a.Tools())
		if err != nil {
			return "", fmt.Errorf("llm: %w", err)
		}
		s.messages = append(s.messages, *am)

		if len(am.ToolCalls) == 0 {
			return am.Text, nil
		}

		if am.Text != "" && progress != nil {
			progress("💭 " + am.Text)
		}

		// 逐个执行工具调用（顺序执行，结果立即回传）
		for _, tc := range am.ToolCalls {
			result := s.callTool(tc, progress)
			s.messages = append(s.messages, llm.Message{
				Role: "tool", ToolCallID: tc.ID, ToolResult: result,
			})
		}
	}
	return "", fmt.Errorf("reached max agent turns (%d); aborting to avoid runaway loop", maxTurns)
}

func (s *Session) callTool(tc llm.ToolCall, progress func(line string)) string {
	a := s.agent

	var args map[string]any
	if err := json.Unmarshal([]byte(defaultJSONStr(tc.Arguments)), &args); err != nil {
		return toolErr(tc, "invalid tool arguments JSON: "+err.Error())
	}

	a.mu.RLock()
	serverName, ok := a.routes[tc.Name]
	client := a.clients[serverName]
	origName := a.origNames[tc.Name]
	a.mu.RUnlock()
	if !ok || client == nil {
		return toolErr(tc, fmt.Sprintf("unknown tool %q; call /tools to list available tools", tc.Name))
	}
	if origName == "" {
		origName = tc.Name
	}

	if progress != nil {
		progress(fmt.Sprintf("🔧 %s.%s(%s)", serverName, tc.Name, compactJSON(args)))
	}

	res, err := client.CallTool(origName, args)
	if err != nil {
		return toolErr(tc, err.Error())
	}
	text := res.ResultText()
	if res.IsError {
		if progress != nil {
			progress("   ⚠️  " + firstLine(text, 200))
		}
		return text // 工具级错误也回传给模型自纠
	}
	if progress != nil {
		progress("   ✅ " + firstLine(text, 200))
	}
	return text
}

func toolErr(tc llm.ToolCall, msg string) string {
	return fmt.Sprintf("TOOL ERROR (%s): %s", tc.Name, msg)
}

func compactJSON(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "?"
	}
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

func defaultJSONStr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	return s
}

func ctxBG() context.Context { return context.Background() }
