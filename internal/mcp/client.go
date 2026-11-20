package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client 是 host 侧的 MCP 客户端：连接一个自研 MCP Server
// （Streamable HTTP），完成 initialize 握手后调用 tools/list 与 tools/call。
type Client struct {
	name     string // 展示名，例如 "ssh" / "db"
	baseURL  string // 例如 http://127.0.0.1:8801/mcp
	token    string
	http     *http.Client
	sessID   atomic.Value // string
	nextID   atomic.Int64
}

func NewClient(name, baseURL, token string) *Client {
	return &Client{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 300 * time.Second}, // 整体上限，工具级超时由 host 控制
	}
}

// Name 返回展示名（实现 ToolServer 接口）。
func (c *Client) Name() string { return c.name }

// Close 实现 ToolServer 接口。HTTP 无持久连接需关闭，空操作。
func (c *Client) Close() error { return nil }

// Initialize 执行握手并保存 session id
func (c *Client) Initialize() error {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    json.RawMessage(`{}`),
		ClientInfo:      Implementation{Name: "mcp-ops-host", Version: "0.3.0"},
	}
	var result InitializeResult
	hdr, err := c.call(MethodInitialize, params, &result)
	if err != nil {
		return fmt.Errorf("initialize %s: %w", c.name, err)
	}
	if sid := hdr.Get("Mcp-Session-Id"); sid != "" {
		c.sessID.Store(sid)
	}
	if result.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("server %s protocol %q != %q", c.name, result.ProtocolVersion, ProtocolVersion)
	}
	// 发送 initialized 通知（协议要求；服务器返回 202）
	_ = c.notify(MethodInitialized, nil)
	return nil
}

// ListTools 拉取工具清单
func (c *Client) ListTools() ([]Tool, error) {
	var result ListToolsResult
	if _, err := c.call(MethodToolsList, map[string]any{}, &result); err != nil {
		return nil, fmt.Errorf("tools/list %s: %w", c.name, err)
	}
	return result.Tools, nil
}

// CallTool 调用工具并返回文本结果
func (c *Client) CallTool(name string, args map[string]any) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := ToolsCallParams{Name: name, Arguments: args}
	var result CallToolResult
	if _, err := c.call(MethodToolsCall, params, &result); err != nil {
		return nil, fmt.Errorf("tools/call %s.%s: %w", c.name, name, err)
	}
	return &result, nil
}

// ResultText 把 CallToolResult 的所有 text 块拼成一个字符串
func (r *CallToolResult) ResultText() string {
	var sb strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

func (c *Client) call(method string, params any, result any) (http.Header, error) {
	hdr, resp, err := c.rpc(method, params, false)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return hdr, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Result) > 0 && result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return hdr, fmt.Errorf("decode result: %w", err)
		}
	}
	return hdr, nil
}

func (c *Client) notify(method string, params any) error {
	_, _, err := c.rpc(method, params, true)
	return err
}

func (c *Client) rpc(method string, params any, isNotification bool) (http.Header, *Response, error) {
	raw, _ := json.Marshal(params)
	req := Request{JSONRPC: JSONRPCVersion, Method: method, Params: raw}
	if !isNotification {
		req.ID = json.RawMessage(fmt.Sprintf(`%d`, c.nextID.Add(1)))
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL, bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	if sid, ok := c.sessID.Load().(string); ok && sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer httpResp.Body.Close()
	hdr := httpResp.Header

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 64<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}
	if isNotification {
		if httpResp.StatusCode >= 400 {
			return hdr, nil, fmt.Errorf("notification %s: http %d", method, httpResp.StatusCode)
		}
		return hdr, nil, nil
	}
	if httpResp.StatusCode == http.StatusUnauthorized {
		return hdr, nil, fmt.Errorf("unauthorized: check token")
	}
	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return hdr, nil, fmt.Errorf("decode response (http %d): %w", httpResp.StatusCode, err)
	}
	return hdr, &resp, nil
}
