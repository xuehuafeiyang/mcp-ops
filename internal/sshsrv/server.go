package sshsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"mcp-ops/internal/mcp"
)

// RegisterTools 把 SSH 工具注册到 MCP Server
func RegisterTools(s *mcp.Server, pool *Pool) {
	s.RegisterTool("list_ssh_hosts",
		"列出所有已配置的 SSH 服务器：名称、地址、用户、允许的命令前缀白名单、超时。",
		json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		func(mcp.CallContext, map[string]any) *mcp.CallToolResult {
			hosts := pool.cfg.Get().SSHHosts
			var out []map[string]any
			for _, h := range hosts {
				out = append(out, map[string]any{
					"name":             h.Name,
					"host":             net.JoinHostPort(h.Host, fmt.Sprintf("%d", h.Port)),
					"user":             h.User,
					"allowed_commands": h.AllowedCommands,
					"timeout_seconds":  int(h.Timeout().Seconds()),
				})
			}
			if out == nil {
				out = []map[string]any{}
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			return mcp.TextResult(string(b))
		})

	s.RegisterTool("ssh_run",
		"在指定 SSH 服务器上执行一条命令（必须匹配该主机的命令白名单，危险命令一律被拒绝）。返回 combined stdout/stderr 与退出码。",
		json.RawMessage(`{"type":"object","properties":{"host":{"type":"string","description":"SSH 主机名称，先调 list_ssh_hosts 查看"},"command":{"type":"string","description":"要执行的完整命令行"}},"required":["host","command"]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			hostName, _ := args["host"].(string)
			command, _ := args["command"].(string)

			host := pool.cfg.Get().FindSSHHost(hostName)
			if host == nil {
				return mcp.ErrResult(fmt.Sprintf("ssh host %q not found", hostName))
			}
			if err := CheckPolicy(host, command); err != nil {
				return mcp.ErrResult("POLICY DENIED: " + err.Error())
			}

			timeout := host.Timeout()
			cctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			r := runCommand(cctx, pool, hostName, command)
			if cctx.Err() == context.DeadlineExceeded {
				return mcp.ErrResult(fmt.Sprintf("TIMEOUT: command exceeded %s (remote session terminated)", timeout))
			}
			if r.err != nil {
				return mcp.ErrResult("EXEC ERROR: " + r.err.Error())
			}
			out := fmt.Sprintf("exit_code=%d\n--- output ---\n%s", r.code, truncate(r.output, 64*1024))
			return mcp.TextResult(out)
		})
}

type runResult struct {
	code   int
	output string
	err    error
}

func runCommand(ctx context.Context, pool *Pool, hostName, command string) runResult {
	client, err := pool.Get(hostName)
	if err != nil {
		return runResult{err: err}
	}
	session, err := client.NewSession()
	if err != nil {
		return runResult{err: fmt.Errorf("new session: %w", err)}
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Start(command); err != nil {
		return runResult{err: fmt.Errorf("start: %w", err)}
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- session.Wait() }()

	combine := func() string {
		out := stdout.String()
		if e := stderr.String(); e != "" {
			out += e
		}
		return strings.TrimRight(out, "\n")
	}

	select {
	case <-ctx.Done():
		// 超时：先向远端进程发 SIGKILL，再关 channel，尽量不留孤儿进程
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return runResult{code: -1, output: combine()}
	case werr := <-waitCh:
		code := 0
		if werr != nil {
			if ee, ok := werr.(*ssh.ExitError); ok {
				code = ee.ExitStatus()
			} else {
				return runResult{code: 1, output: combine(), err: fmt.Errorf("run: %w", werr)}
			}
		}
		return runResult{code: code, output: combine()}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [output truncated]"
}

// DialTunnel 经 SSH 主机建立 TCP 隧道，返回到 targetHost:targetPort 的连接。
// 供 DB Server 复用（via_ssh 数据库连接走这里）。
func DialTunnel(pool *Pool, sshHost, targetHost string, targetPort int) (net.Conn, error) {
	client, err := pool.Get(sshHost)
	if err != nil {
		return nil, err
	}
	return client.Dial("tcp", net.JoinHostPort(targetHost, fmt.Sprintf("%d", targetPort)))
}
