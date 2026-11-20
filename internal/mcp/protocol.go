// Package mcp 实现精简但协议正确的 MCP 层：
// JSON-RPC 2.0 over Streamable HTTP（客户端 POST 单请求，服务端 JSON 单响应）。
// 同时提供 server 端（自研 SSH/DB Server 复用）与 client 端（host 复用）。
//
// 覆盖协议能力：tools、resources、prompts、progress、roots、logging。
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ProtocolVersion   = "2025-03-26"
	JSONRPCVersion    = "2.0"
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodPing        = "ping"

	// tools
	MethodToolsList = "tools/list"
	MethodToolsCall = "tools/call"

	// resources
	MethodResourcesList          = "resources/list"
	MethodResourcesRead          = "resources/read"
	MethodResourcesSubscribe     = "resources/subscribe"
	MethodResourcesTemplatesList = "resources/templates/list"

	// prompts
	MethodPromptsList = "prompts/list"
	MethodPromptsGet  = "prompts/get"

	// 客户端能力协商
	MethodRootsList = "roots/list"

	// 服务端日志
	MethodLoggingSetLevel = "logging/setLevel"

	// notifications
	MethodNotificationsProgress  = "notifications/progress"
	MethodNotificationsCancelled = "notifications/cancelled"
	MethodNotificationsMessage   = "notifications/message"
	MethodToolsListChanged       = "notifications/tools/list_changed"
	MethodResourcesListChanged   = "notifications/resources/list_changed"
	MethodPromptsListChanged     = "notifications/prompts/list_changed"
)

// ---------- JSON-RPC 2.0 ----------

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // string | number；空表示 notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// ---------- MCP 核心类型 ----------

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      Implementation  `json:"clientInfo"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    ServerCaps     `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
}

type ServerCaps struct {
	Tools *struct {
		ListChanged bool `json:"listChanged,omitempty"`
	} `json:"tools,omitempty"`
	Resources *struct {
		Subscribe   bool `json:"subscribe,omitempty"`
		ListChanged bool `json:"listChanged,omitempty"`
	} `json:"resources,omitempty"`
	Prompts *struct {
		ListChanged bool `json:"listChanged,omitempty"`
	} `json:"prompts,omitempty"`
	Logging *struct{} `json:"logging,omitempty"`
}

// ---------- Tools ----------

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"` // JSON Schema object
}

type ToolHandler func(ctx CallContext, args map[string]any) *CallToolResult

// CallContext 携带请求上下文：取消信号与进度回传。
type CallContext struct {
	Cancel <-chan struct{} // 请求取消信号
	// Progress 用于工具内向客户端回报进度（可选，nil 表示不回报）。
	Progress func(progress, total float64, message string)
}

type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

type ToolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// ProgressToken 兼容旧版 spec（顶层 progressToken）。
	ProgressToken any `json:"progressToken,omitempty"`
	// Meta 承载 progressToken 等协议元信息（新版 spec 用 _meta）。
	Meta map[string]any `json:"_meta,omitempty"`
}

// ProgressToken 从 params 中解析（兼容顶层与 _meta 两种位置）。
func (p *ToolsCallParams) token() any {
	if p.ProgressToken != nil {
		return p.ProgressToken
	}
	if p.Meta != nil {
		if t, ok := p.Meta["progressToken"]; ok && t != nil {
			return t
		}
	}
	return nil
}

// Content 是 MCP 内容块。Type 支持：
//
//	text              纯文本（Text）
//	image / audio     base64 二进制（Data + MimeType）
//	resource          resource_link（URI + MimeType + 可选 Text）
//	embedded_resource 内嵌资源（Resource）
type Content struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`     // image/audio: base64
	MimeType string            `json:"mimeType,omitempty"` // image/audio/resource_link
	URI      string            `json:"uri,omitempty"`      // resource_link
	Name     string            `json:"name,omitempty"`     // resource_link 显示名
	Resource *EmbeddedResource `json:"resource,omitempty"` // embedded_resource
}

// EmbeddedResource 内嵌在工具结果里的资源内容。
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"` // base64
}

type CallToolResult struct {
	Content           []Content `json:"content"`
	IsError           bool      `json:"isError,omitempty"`
	StructuredContent any       `json:"structuredContent,omitempty"`
}

func TextResult(s string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: s}}}
}

// ImageResult 返回一张图片内容块（data 为 base64）。
func ImageResult(data, mimeType string) *CallToolResult {
	if mimeType == "" {
		mimeType = "image/png"
	}
	return &CallToolResult{Content: []Content{{Type: "image", Data: data, MimeType: mimeType}}}
}

// ResourceLinkResult 返回一个资源链接内容块。
func ResourceLinkResult(uri, mimeType, text string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "resource", URI: uri, MimeType: mimeType, Text: text}}}
}

// EmbeddedResourceResult 返回一个内嵌资源内容块。
func EmbeddedResourceResult(res EmbeddedResource) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "embedded_resource", Resource: &res}}}
}

func ErrResult(err string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: err}}, IsError: true}
}

// ResultText 把所有 text 块拼成一个字符串；非 text 块给占位说明。
func (r *CallToolResult) ResultText() string {
	var sb strings.Builder
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		case "image":
			fmt.Fprintf(&sb, "\n[image: %s, %d bytes base64]", c.MimeType, len(c.Data))
		case "audio":
			fmt.Fprintf(&sb, "\n[audio: %s, %d bytes base64]", c.MimeType, len(c.Data))
		case "resource":
			fmt.Fprintf(&sb, "\n[resource: %s (%s)]", c.URI, c.MimeType)
			if c.Text != "" {
				sb.WriteString(" " + c.Text)
			}
		case "embedded_resource":
			if c.Resource != nil {
				fmt.Fprintf(&sb, "\n[embedded_resource: %s (%s)]", c.Resource.URI, c.Resource.MimeType)
				if c.Resource.Text != "" {
					sb.WriteString(" " + c.Resource.Text)
				}
			}
		}
	}
	return strings.TrimPrefix(sb.String(), "\n")
}

// ---------- Resources ----------

type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Text        string `json:"text,omitempty"` // 静态资源：文本内容
	Blob        string `json:"blob,omitempty"` // 静态资源：base64 二进制
}

// ResourceContents 是 resources/read 返回的内容。
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

type ListResourcesResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type ReadResourceParams struct {
	URI string `json:"uri"`
}

type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
}

type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type ListResourceTemplatesResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

// ResourceHandler 动态资源读取器：uri 为未展开的模板实例。
type ResourceHandler func(ctx CallContext, uri string) (*ResourceContents, error)

// ---------- Prompts ----------

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

type PromptMessage struct {
	Role    string  `json:"role"` // "user" | "assistant"
	Content Content `json:"content"`
}

type ListPromptsResult struct {
	Prompts []Prompt `json:"prompts"`
}

type GetPromptParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// PromptHandler 动态生成提示词消息；args 为 prompts/get 传入的参数。
type PromptHandler func(ctx CallContext, args map[string]any) ([]PromptMessage, error)

// ---------- Progress / Roots / Logging ----------

type ProgressParams struct {
	ProgressToken any     `json:"progressToken"`
	Progress      float64 `json:"progress"`
	Total         float64 `json:"total,omitempty"`
	Message       string  `json:"message,omitempty"`
}

type RootsListResult struct {
	Roots []Root `json:"roots"`
}

type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

type LoggingSetLevelParams struct {
	Level string `json:"level"` // debug | info | notice | warning | error | critical | alert | emergency
}
