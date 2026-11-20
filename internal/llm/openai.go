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

// OpenAI 兼容适配器：覆盖 OpenAI / DeepSeek / Qwen / GLM / Kimi / Ollama / vLLM 等。
type openaiProvider struct {
	cfg OpenAIConfig
	cli *http.Client
}

func NewOpenAI(cfg *OpenAIConfig) Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	return &openaiProvider{
		cfg: *cfg,
		cli: &http.Client{Timeout: 180 * time.Second},
	}
}

func (p *openaiProvider) Name() string { return "openai:" + p.cfg.Model }

// ---------- 线上格式 ----------

type oaMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"` // "function"
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaTool struct {
	Type     string      `json:"type"` // "function"
	Function oaFunctionD `json:"function"`
}

type oaFunctionD struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaRequest struct {
	Model    string      `json:"model"`
	Messages []oaMessage `json:"messages"`
	Tools    []oaTool    `json:"tools,omitempty"`
}

type oaResponse struct {
	Choices []struct {
		Message struct {
			Role      string       `json:"role"`
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *openaiProvider) Chat(ctx context.Context, system string, msgs []Message, tools []mcp.Tool) (*Message, error) {
	oam := make([]oaMessage, 0, len(msgs)+1)
	if system != "" {
		oam = append(oam, oaMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			oam = append(oam, oaMessage{Role: "user", Content: m.Text})
		case "assistant":
			om := oaMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, oaToolCall{
					ID: tc.ID, Type: "function",
					Function: oaFunction{Name: tc.Name, Arguments: tc.Arguments},
				})
			}
			oam = append(oam, om)
		case "tool":
			oam = append(oam, oaMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: m.ToolResult})
		}
	}

	var oaTools []oaTool
	for _, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		oaTools = append(oaTools, oaTool{
			Type:     "function",
			Function: oaFunctionD{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}

	body, _ := json.Marshal(oaRequest{Model: p.cfg.Model, Messages: oam, Tools: oaTools})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	resp, err := p.cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("openai http %d: %s", resp.StatusCode, truncate(string(data), 512))
	}
	var out oaResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode openai response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("openai error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty choices")
	}
	c := out.Choices[0].Message
	am := &Message{Role: "assistant", Text: c.Content}
	for _, tc := range c.ToolCalls {
		if tc.Type != "function" && tc.Type != "" {
			continue
		}
		am.ToolCalls = append(am.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return am, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
