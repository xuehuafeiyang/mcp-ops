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

	callTimeout time.Duration
}

// NewStdioClient 构造（不启动进程，Initialize 时才启动）。
func NewStdioClient(name, command string, args []string, env map[string]string) *StdioClient {
	c := &StdioClient{
		name:        name,
		command:     command,
		args:        args,
		pending:     map[string]chan *Response{},
		callTimeout: 300 * time.Second, // 工具级上限；npx 首次拉包可能较慢
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
	case <-time.After(c.callTimeout):
		c.fail(key)
		return fmt.Errorf("%s: %s 超时（%s）", c.name, method, c.callTimeout)
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
