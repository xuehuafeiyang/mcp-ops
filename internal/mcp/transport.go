package mcp

// ToolServer 是 Agent 抽象的工具来源：无论工具来自
//   - 远程 Streamable HTTP server（*Client）
//   - stdio 子进程（*StdioClient，如 npx 拉起的官方 server）
//   - 进程内注册（*InProcServer，内置 SSH/DB/Web）
//
// 都提供「握手 → 拉工具清单 → 调工具 → 关闭」这套统一操作。
type ToolServer interface {
	Name() string
	Initialize() error
	ListTools() ([]Tool, error)
	CallTool(name string, args map[string]any) (*CallToolResult, error)
	Close() error
}
