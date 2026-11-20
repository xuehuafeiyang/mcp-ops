// Package mcp 实现精简但协议正确的 MCP 层：
// JSON-RPC 2.0 over Streamable HTTP（客户端 POST 单请求，服务端 JSON 单响应）。
// 同时提供 server 端（自研 SSH/DB Server 复用）与 client 端（host 复用）。
package mcp

import "encoding/json"

const (
	ProtocolVersion  = "2025-03-26"
	JSONRPCVersion    = "2.0"
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodToolsList   = "tools/list"
	MethodToolsCall   = "tools/call"
	MethodPing        = "ping"
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
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    ServerCaps      `json:"capabilities"`
	ServerInfo      Implementation  `json:"serverInfo"`
}

type ServerCaps struct {
	Tools *struct {
		ListChanged bool `json:"listChanged,omitempty"`
	} `json:"tools,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"` // JSON Schema object
}

type ToolHandler func(ctx CallContext, args map[string]any) *CallToolResult

type CallContext struct {
	Cancel <-chan struct{} // 请求取消信号
}

type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

type ToolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type Content struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func TextResult(s string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: s}}}
}

func ErrResult(err string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: err}}, IsError: true}
}
