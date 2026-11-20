package config

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Load 从 YAML 文件加载配置
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = "openai"
	}
	if cfg.LLM.MaxTurns == 0 {
		cfg.LLM.MaxTurns = 30
	}
	for i := range cfg.SSHHosts {
		if cfg.SSHHosts[i].Port == 0 {
			cfg.SSHHosts[i].Port = 22
		}
	}
	if cfg.LLM.SystemPrompt == "" {
		cfg.LLM.SystemPrompt = DefaultSystemPrompt
	}
	return &cfg, nil
}

const DefaultSystemPrompt = `你是一个服务器与数据库运维助手，通过 MCP 工具操作多台服务器和多个数据库。
规则：
1. 操作前先了解目标（用 list_ssh_hosts / list_databases 查看可用目标）。
2. 服务器命令必须与该主机白名单前缀匹配，被拒绝时换一个等价的允许命令。
3. 数据库查询返回结果过大时主动加 LIMIT 收敛。
4. 每一步用中文简短说明你要做什么、为什么。`

// Manager 提供配置热重载：SIGHUP 信号 + mtime 轮询双保险，
// 通过 atomic.Pointer 保证并发读安全（读端永远拿到完整快照）。
type Manager struct {
	path   string
	cur    atomic.Pointer[Config]
	stop   chan struct{}
	mu     sync.Mutex
	onChg  []func(*Config)
	stopped bool
}

func NewManager(path string, cfg *Config) *Manager {
	m := &Manager{path: path, stop: make(chan struct{})}
	m.cur.Store(cfg)
	return m
}

// LoadAndWatch 加载配置并启动热重载监听
func LoadAndWatch(path string) (*Manager, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Load(abs)
	if err != nil {
		return nil, err
	}
	m := NewManager(abs, cfg)
	go m.watch()
	return m, nil
}

func (m *Manager) Get() *Config { return m.cur.Load() }

// OnChange 注册配置变更回调（在 watcher goroutine 中调用）
func (m *Manager) OnChange(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChg = append(m.onChg, fn)
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stopped {
		m.stopped = true
		close(m.stop)
	}
}

func (m *Manager) watch() {
	// SIGHUP 立即重载
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	var lastMod = m.modTime()
	for {
		select {
		case <-m.stop:
			return
		case <-sigCh:
			m.reload()
		case <-tick.C:
			if mt := m.modTime(); !mt.Equal(lastMod) {
				lastMod = mt
				m.reload()
			}
		}
	}
}

func (m *Manager) modTime() time.Time {
	st, err := os.Stat(m.path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func (m *Manager) reload() {
	cfg, err := Load(m.path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[config] reload failed: %v (keep old config)\n", err)
		return
	}
	m.cur.Store(cfg)
	m.mu.Lock()
	fns := append([]func(*Config){}, m.onChg...)
	m.mu.Unlock()
	for _, fn := range fns {
		fn(cfg) // 回调内自行处理错误，不能 panic
	}
	fmt.Fprintf(os.Stderr, "[config] reloaded %s\n", m.path)
}
