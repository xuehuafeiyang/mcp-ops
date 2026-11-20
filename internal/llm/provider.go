// Package llm 统一 LLM Provider 抽象：
// host 的 Agent 循环只面对此包的消息模型，OpenAI 兼容与 Anthropic 原生
// 两种 function-calling 格式的转换都封装在各适配器内部。
package llm

import (
	"context"
	"fmt"

	"mcp-ops/internal/mcp"
)

// Message 是 host 侧的统一消息表示。
//   - Role=user: Text 有效
//   - Role=assistant: Text 与 ToolCalls 可同时有效（模型边说边调工具）
//   - Role=tool: ToolCallID + ToolResult 有效（工具结果回传）
type Message struct {
	Role       string
	Text       string
	ToolCalls  []ToolCall
	ToolCallID string
	ToolResult string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON 字符串
}

// Provider 对接一个大模型 API
type Provider interface {
	Name() string
	// Chat 发起一轮对话；tools 来自各 MCP server 的 tools/list。
	// 返回的 assistant Message 可能含 ToolCalls（由 host 决定执行后继续）。
	Chat(ctx context.Context, system string, msgs []Message, tools []mcp.Tool) (*Message, error)
}

// New 按配置构造 Provider
func New(providerType string, mk func() (*OpenAIConfig, error), ma func() (*AnthropicConfig, error)) (Provider, error) {
	switch providerType {
	case "openai":
		c, err := mk()
		if err != nil {
			return nil, err
		}
		return NewOpenAI(c), nil
	case "anthropic":
		c, err := ma()
		if err != nil {
			return nil, err
		}
		return NewAnthropic(c), nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q (openai | anthropic)", providerType)
	}
}

// OpenAIConfig / AnthropicConfig 由 config 包结构平铺传入，避免反向依赖
type OpenAIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

type AnthropicConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}
