package dbsrv

import (
	"fmt"
	"io"
	"net"
	"sync"

	"mcp-ops/internal/config"
	"mcp-ops/internal/sshsrv"
)

// tunnel 是一个本地端口转发（等价 ssh -L）：
// 127.0.0.1:随机端口 → SSH 主机 → 目标数据库。
// 驱动只看到本地地址，对各数据库驱动完全透明。
type tunnel struct {
	listener net.Listener
	stop     chan struct{}
}

var (
	tunnelMu sync.Mutex
	tunnels  = map[string]*tunnel{}
)

func (m *Manager) getTunnel(db *config.Database) *tunnel {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if t, ok := tunnels[db.Name]; ok {
		return t
	}
	t := newTunnel(m.pool, db)
	if t != nil {
		tunnels[db.Name] = t
	}
	return t
}

func closeAllTunnels() {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	for n, t := range tunnels {
		t.close()
		delete(tunnels, n)
	}
}

func (t *tunnel) localAddr() string {
	if t == nil {
		return ""
	}
	return t.listener.Addr().String()
}

func (t *tunnel) close() {
	if t == nil {
		return
	}
	select {
	case <-t.stop:
		return
	default:
	}
	close(t.stop)
	_ = t.listener.Close()
}

func newTunnel(pool *sshsrv.Pool, db *config.Database) *tunnel {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil
	}
	t := &tunnel{listener: ln, stop: make(chan struct{})}
	target := fmt.Sprintf("%s:%d", db.Host, db.Port)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case <-t.stop:
				_ = conn.Close()
				return
			default:
			}
			go t.forward(pool, db.ViaSSH, target, conn)
		}
	}()
	return t
}

func (t *tunnel) forward(pool *sshsrv.Pool, sshHost, target string, local net.Conn) {
	remote, err := sshsrv.DialTunnel(pool, sshHost, host(target), portOf(target))
	if err != nil {
		_ = local.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(remote, local); done <- struct{}{} }()
	go func() { io.Copy(local, remote); done <- struct{}{} }()
	<-done
	_ = local.Close()
	_ = remote.Close()
}

func host(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	var port int
	fmt.Sscanf(p, "%d", &port)
	return port
}
