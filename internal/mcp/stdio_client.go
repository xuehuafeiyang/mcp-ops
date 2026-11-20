package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// StdioClient 是 host 侧的 stdio 传输 MCP 客户端：
// 用 command + args 启动一个子进程，通过 stdin/stdout 走
// newline-delimited JSON-RPC 与它通信。
//
// 这让 opsctl 能挂载任意 stdio MCP server —— 例如 npx 拉起的官方
// @modelcontextprotocol/server-filesystem / server-memory，或本机安装的
// 任意社区/自建命令。
type StdioClient struct {
	name    string
	command string
	args    []string
	env     []string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu      sync.Mutex
	nextID  atomic.Int64
	pending map[string]chan *Response
	closed  bool

	callTimeout  time.Duration
	probeTimeout time.Duration // resources/prompts 等协议探测调用的短超时
	caps         ServerCaps    // 握手时 server 声明的能力
}

// NewStdioClient 构造（不启动进程，Initialize 时才启动）。
func NewStdioClient(name, command string, args []string, env map[string]string) *StdioClient {
	c := &StdioClient{
		name:         name,
		command:      command,
		args:         args,
		pending:      map[string]chan *Response{},
		callTimeout:  300 * time.Second, // 工具级上限；npx 首次拉包可能较慢
		probeTimeout: 10 * time.Second,  // resources/prompts 探测：不响应的 server 不能拖死注册流程
	}
	for k, v := range env {
		c.env = append(c.env, k+"="+v)
	}
	return c
}

func (c *StdioClient) Name() string { return c.name }

// Initialize 启动子进程并完成 MCP 握手。
func (c *StdioClient) Initialize() error {
	cmd := exec.Command(c.command, c.args...)
	cmd.Env = append(os.Environ(), c.env...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: stderr pipe: %w", c.name, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%s: stdin pipe: %w", c.name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: stdout pipe: %w", c.name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: 启动失败 (%s %v): %w", c.name, c.command, c.args, err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stdout = bufio.NewReaderSize(stdout, 1<<20)

	go c.drainStderr(stderr)
	go c.readLoop()

	var result InitializeResult
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    json.RawMessage(`{}`), // 新版 server（Zod 校验）要求必须是对象
		ClientInfo:      Implementation{Name: "mcp-ops-host", Version: "0.3.0"},
	}
	if err := c.call(MethodInitialize, params, &result); err != nil {
		c.Close()
		return err
	}
	if result.ProtocolVersion != ProtocolVersion {
		c.Close()
		return fmt.Errorf("%s: protocol %q != %q", c.name, result.ProtocolVersion, ProtocolVersion)
	}
	c.caps = result.Capabilities // 记录 server 声明的能力，后续探测据此决定是否发请求
	// 通知服务端握手完成（协议要求）
	if err := c.notify(MethodInitialized, nil); err != nil {
		c.Close()
		return fmt.Errorf("%s: initialized notify: %w", c.name, err)
	}
	return nil
}

func (c *StdioClient) ListTools() ([]Tool, error) {
	var result ListToolsResult
	if err := c.call(MethodToolsList, map[string]any{}, &result); err != nil {
		return nil, fmt.Errorf("%s tools/list: %w", c.name, err)
	}
	return result.Tools, nil
}

func (c *StdioClient) CallTool(name string, args map[string]any) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := ToolsCallParams{Name: name, Arguments: args}
	var result CallToolResult
	if err := c.call(MethodToolsCall, params, &result); err != nil {
		return nil, fmt.Errorf("%s.%s: %w", c.name, name, err)
	}
	return &result, nil
}

// ListResources 拉取资源清单。仅当握手时 server 声明了 resources 能力才发请求。
func (c *StdioClient) ListResources() ([]Resource, error) {
	if c.caps.Resources == nil {
		return nil, nil
	}
	var result ListResourcesResult
	if err := c.callIn(MethodResourcesList, map[string]any{}, &result, c.probeTimeout); err != nil {
		return nil, fmt.Errorf("%s resources/list: %w", c.name, err)
	}
	return result.Resources, nil
}

// ReadResource 读取单个资源内容。
func (c *StdioClient) ReadResource(uri string) (*ResourceContents, error) {
	if c.caps.Resources == nil {
		return nil, fmt.Errorf("%s 未声明 resources 能力", c.name)
	}
	var result ReadResourceResult
	if err := c.callIn(MethodResourcesRead, ReadResourceParams{URI: uri}, &result, c.probeTimeout); err != nil {
		return nil, fmt.Errorf("%s resources/read: %w", c.name, err)
	}
	if len(result.Contents) == 0 {
		return nil, fmt.Errorf("%s resources/read: empty contents", c.name)
	}
	return &result.Contents[0], nil
}

// ListResourceTemplates 拉取资源模板清单。
func (c *StdioClient) ListResourceTemplates() ([]ResourceTemplate, error) {
	if c.caps.Resources == nil {
		return nil, nil
	}
	var result ListResourceTemplatesResult
	if err := c.callIn(MethodResourcesTemplatesList, map[string]any{}, &result, c.probeTimeout); err != nil {
		return nil, fmt.Errorf("%s resources/templates/list: %w", c.name, err)
	}
	return result.ResourceTemplates, nil
}

// ListPrompts 拉取提示词清单。仅当握手时 server 声明了 prompts 能力才发请求。
func (c *StdioClient) ListPrompts() ([]Prompt, error) {
	if c.caps.Prompts == nil {
		return nil, nil
	}
	var result ListPromptsResult
	if err := c.callIn(MethodPromptsList, map[string]any{}, &result, c.probeTimeout); err != nil {
		return nil, fmt.Errorf("%s prompts/list: %w", c.name, err)
	}
	return result.Prompts, nil
}

// GetPrompt 获取指定提示词。
func (c *StdioClient) GetPrompt(name string, args map[string]any) (*GetPromptResult, error) {
	if c.caps.Prompts == nil {
		return nil, fmt.Errorf("%s 未声明 prompts 能力", c.name)
	}
	if args == nil {
		args = map[string]any{}
	}
	var result GetPromptResult
	if err := c.callIn(MethodPromptsGet, GetPromptParams{Name: name, Arguments: args}, &result, c.probeTimeout); err != nil {
		return nil, fmt.Errorf("%s prompts/get: %w", c.name, err)
	}
	return &result, nil
}

// Close 关闭 stdin 并结束子进程。幂等。
func (c *StdioClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	proc := (*os.Process)(nil)
	if c.cmd != nil {
		proc = c.cmd.Process
	}
	c.mu.Unlock()

	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if proc != nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
	}
	return nil
}

// ---------- 内部 ----------

func (c *StdioClient) call(method string, params any, result any) error {
	return c.callIn(method, params, result, c.callTimeout)
}

// callIn 与 call 相同，但允许指定超时——协议探测类调用用短超时，
// 避免不响应的 server 拖死调用方。
func (c *StdioClient) callIn(method string, params any, result any, timeout time.Duration) error {
	raw, _ := json.Marshal(params)
	id := c.nextID.Add(1)
	req := Request{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
		Params:  raw,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	key := fmt.Sprintf("%d", id)
	ch := make(chan *Response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("%s: client closed", c.name)
	}
	c.pending[key] = ch
	c.mu.Unlock()

	if _, err := c.stdin.Write(data); err != nil {
		c.fail(key)
		return fmt.Errorf("%s: write %s: %w", c.name, method, err)
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return fmt.Errorf("%s: server exited during %s", c.name, method)
		}
		if resp.Error != nil {
			return fmt.Errorf("%s %s rpc error %d: %s", c.name, method, resp.Error.Code, resp.Error.Message)
		}
		if len(resp.Result) > 0 && result != nil {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("%s decode %s result: %w", c.name, method, err)
			}
		}
		return nil
	case <-time.After(timeout):
		c.fail(key)
		return fmt.Errorf("%s: %s 超时（%s）", c.name, method, timeout)
	}
}

func (c *StdioClient) notify(method string, params any) error {
	raw, _ := json.Marshal(params)
	req := Request{JSONRPC: JSONRPCVersion, Method: method, Params: raw}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.stdin.Write(data)
	return err
}

func (c *StdioClient) readLoop() {
	for {
		line, err := c.stdout.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
			var resp Response
			if json.Unmarshal(line, &resp) == nil && len(resp.ID) > 0 {
				c.deliver(string(resp.ID), &resp)
			}
		}
		if err != nil {
			// 进程退出：唤醒所有等待中的调用
			c.mu.Lock()
			for k, ch := range c.pending {
				if ch != nil {
					close(ch)
				}
				delete(c.pending, k)
			}
			c.mu.Unlock()
			return
		}
	}
}

func (c *StdioClient) deliver(key string, resp *Response) {
	c.mu.Lock()
	ch, ok := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if ok && ch != nil {
		ch <- resp
	}
}

func (c *StdioClient) fail(key string) {
	c.mu.Lock()
	ch, ok := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if ok && ch != nil {
		close(ch)
	}
}

func (c *StdioClient) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", c.name, sc.Text())
	}
}
