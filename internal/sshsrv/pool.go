package sshsrv

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"mcp-ops/internal/config"
)

// Pool 维护到所有 SSH 主机的长连接：
// 每台主机一条连接 + keepalive 探活 + 使用时发现失效自动重拨。
type Pool struct {
	cfg  *config.Manager
	mu   sync.Mutex
	conn map[string]*ssh.Client
}

func NewPool(cfg *config.Manager) *Pool {
	return &Pool{cfg: cfg, conn: map[string]*ssh.Client{}}
}

// Get 返回到指定主机的活跃连接；失效或不存在则重拨。
func (p *Pool) Get(name string) (*ssh.Client, error) {
	host := p.cfg.Get().FindSSHHost(name)
	if host == nil {
		return nil, fmt.Errorf("ssh host %q not found in config", name)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conn[name]; ok && isAlive(c) {
		return c, nil
	}
	if c, ok := p.conn[name]; ok {
		_ = c.Close()
		delete(p.conn, name)
	}
	c, err := dial(host)
	if err != nil {
		return nil, fmt.Errorf("connect %s (%s:%d): %w", name, host.Host, host.Port, err)
	}
	p.conn[name] = c
	go keepalive(c, func() { p.drop(name) })
	return c, nil
}

func (p *Pool) drop(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conn[name]; ok {
		_ = c.Close()
		delete(p.conn, name)
	}
}

// CloseAll 关闭全部连接（进程退出时调用）
func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for n, c := range p.conn {
		_ = c.Close()
		delete(p.conn, n)
	}
}

func isAlive(c *ssh.Client) bool {
	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// keepalive 每 30 秒探活一次，连续失败即关闭连接并触发 drop
func keepalive(c *ssh.Client, drop func()) {
	const interval = 30 * time.Second
	const maxFail = 3
	fails := 0
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			fails++
			if fails >= maxFail {
				_ = c.Close()
				drop()
				return
			}
		} else {
			fails = 0
		}
	}
}

func dial(h *config.SSHHost) (*ssh.Client, error) {
	var methods []ssh.AuthMethod
	added := false

	if h.Auth.PrivateKeyData != "" {
		signer, err := ssh.ParsePrivateKey([]byte(h.Auth.PrivateKeyData))
		if err != nil {
			return nil, fmt.Errorf("parse private_key_data: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
		added = true
	}
	if h.Auth.PrivateKey != "" {
		path := expandHome(h.Auth.PrivateKey)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read private key %s: %w", path, err)
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse private key %s: %w", path, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
		added = true
	}
	if h.Auth.Password != "" {
		methods = append(methods, ssh.Password(h.Auth.Password))
		added = true
	}
	if h.Auth.Agent {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			ag, err := net.Dial("unix", sock)
			if err != nil {
				return nil, fmt.Errorf("connect ssh-agent %s: %w", sock, err)
			}
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(ag).Signers))
			added = true
		} else {
			return nil, fmt.Errorf("agent auth requested but SSH_AUTH_SOCK is empty")
		}
	}
	if !added {
		return nil, fmt.Errorf("host %q: no auth method configured (private_key/private_key_data/password/agent)", h.Name)
	}

	cc := ssh.ClientConfig{
		User:            h.User,
		Auth:            methods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 开发/测试环境：不校验 host key；上生产前必须替换
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(h.Host, fmt.Sprintf("%d", h.Port))
	return ssh.Dial("tcp", addr, &cc)
}

func expandHome(p string) string {
	if len(p) >= 2 && p[0] == '~' && (p[1] == '/' || p[1] == '\\') {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
