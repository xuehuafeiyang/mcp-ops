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

// CapabilityServer 是扩展能力接口：支持 Resources / Prompts 的
// 工具来源（HTTP client、stdio client）额外实现，host 可用类型断言启用。
type CapabilityServer interface {
	ListResources() ([]Resource, error)
	ReadResource(uri string) (*ResourceContents, error)
	ListResourceTemplates() ([]ResourceTemplate, error)
	ListPrompts() ([]Prompt, error)
	GetPrompt(name string, args map[string]any) (*GetPromptResult, error)
}
