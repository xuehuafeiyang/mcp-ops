package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mcp-ops/internal/mcp"
)

// anthropicProvider 对接 Anthropic Messages API（含原生 tool_use）。
type anthropicProvider struct {
	cfg AnthropicConfig
	cli *http.Client
}

func NewAnthropic(cfg *AnthropicConfig) Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com"
	}
	return &anthropicProvider{
		cfg: *cfg,
		cli: &http.Client{Timeout: 180 * time.Second},
	}
}

func (p *anthropicProvider) Name() string { return "anthropic:" + p.cfg.Model }

// ---------- 线上格式 ----------

type anBlock struct {
	Type      string          `json:"type"` // "text" | "tool_use" | "tool_result"
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anMessage struct {
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anRequest struct {
	Model     string      `json:"model"`
	System    string      `json:"system,omitempty"`
	MaxTokens int         `json:"max_tokens"`
	Messages  []anMessage `json:"messages"`
	Tools     []anTool    `json:"tools,omitempty"`
}

type anResponse struct {
	Content []anBlock `json:"content"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *anthropicProvider) Chat(ctx context.Context, system string, msgs []Message, tools []mcp.Tool) (*Message, error) {
	anm := make([]anMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "user":
			anm = append(anm, anMessage{Role: "user", Content: []anBlock{{Type: "text", Text: m.Text}}})
		case "assistant":
			var blocks []anBlock
			if m.Text != "" {
				blocks = append(blocks, anBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name,
					Input: json.RawMessage(defaultJSON(tc.Arguments)),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anBlock{Type: "text", Text: "(empty)"})
			}
			anm = append(anm, anMessage{Role: "assistant", Content: blocks})
		case "tool":
			// Anthropic 要求 tool_result 块挂在下一条 user 消息里
			// 这里约定 host 侧连续 tool 消息会被追加进同一条 user 消息
			if len(anm) > 0 && anm[len(anm)-1].Role == "user" && len(anm[len(anm)-1].Content) > 0 &&
				anm[len(anm)-1].Content[0].Type == "tool_result" {
				anm[len(anm)-1].Content = append(anm[len(anm)-1].Content, anBlock{
					Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.ToolResult,
				})
			} else {
				anm = append(anm, anMessage{Role: "user", Content: []anBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.ToolResult,
				}}})
			}
		}
	}

	var anTools []anTool
	for _, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		anTools = append(anTools, anTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}

	body, _ := json.Marshal(anRequest{
		Model: p.cfg.Model, System: system, MaxTokens: 8192,
		Messages: anm, Tools: anTools,
	})
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("anthropic http %d: %s", resp.StatusCode, truncate(string(data), 512))
	}
	var out anResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("anthropic error: %s", out.Error.Message)
	}

	am := &Message{Role: "assistant"}
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			if am.Text != "" {
				am.Text += "\n"
			}
			am.Text += b.Text
		case "tool_use":
			am.ToolCalls = append(am.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: string(b.Input)})
		}
	}
	return am, nil
}

func defaultJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	return s
}
