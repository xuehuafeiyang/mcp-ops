// Package dbsrv 数据库 MCP Server：统一 Driver 抽象层 +
// MySQL / TiDB / PostgreSQL / 达梦 / 金仓 / 瀚高 / SQLite / MongoDB / Redis / Elasticsearch 实现。
package dbsrv

import (
	"context"
	"fmt"
	"sync"

	"mcp-ops/internal/config"
	"mcp-ops/internal/sshsrv"
)

// Driver 是所有数据库的统一抽象。实现方保证并发安全。
//
//   - SQL 类：Query = SELECT 等（参数化）；Execute = INSERT/UPDATE/DDL
//   - Mongo：Query/Execute 参数为 JSON 描述符（见 mongo_driver.go 注释）
//   - Redis：Query = 读类命令；Execute = 任意命令（全读写模型）
//   - ES：参数为 {"method","path","body"} 的 JSON（REST 直通）
type Driver interface {
	Type() string
	Query(ctx context.Context, q string, params []string) (string, error)
	Execute(ctx context.Context, stmt string, params []string) (string, error)
	Close()
}

// Manager 按配置懒加载驱动；配置热重载后全部重建。
type Manager struct {
	cfg  *config.Manager
	pool *sshsrv.Pool
	mu   sync.Mutex
	drv  map[string]Driver
}

func NewManager(cfg *config.Manager, pool *sshsrv.Pool) *Manager {
	m := &Manager{cfg: cfg, pool: pool, drv: map[string]Driver{}}
	cfg.OnChange(func(*config.Config) { m.CloseAll() })
	return m
}

func (m *Manager) Get(name string) (Driver, error) {
	db := m.cfg.Get().FindDatabase(name)
	if db == nil {
		return nil, fmt.Errorf("database %q not found in config", name)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.drv[name]; ok {
		return d, nil
	}
	d, err := m.open(db)
	if err != nil {
		return nil, fmt.Errorf("open %s (%s): %w", name, db.Type, err)
	}
	m.drv[name] = d
	return d, nil
}

func (m *Manager) open(db *config.Database) (Driver, error) {
	addr := m.address(db) // via_ssh 时建立本地隧道并返回本地地址
	switch db.Type {
	case "mysql":
		return openSQL("mysql", db, addr)
	case "tidb":
		return openSQL("tidb", db, addr)
	case "sqlite", "sqlite3":
		return openSQL("sqlite", db, addr)
	case "postgres", "postgresql":
		return openSQL("postgres", db, addr)
	case "dm", "dameng":
		return openSQL("dm", db, addr)
	case "kingbase", "kingbasees":
		return openSQL("kingbase", db, addr)
	case "highgo", "highgodb":
		return openSQL("highgo", db, addr)
	case "mongodb", "mongo":
		return openMongo(db, addr)
	case "redis":
		return openRedis(db, addr)
	case "elasticsearch", "es":
		return openES(db, addr)
	default:
		return nil, fmt.Errorf("unsupported database type %q (supported: mysql, tidb, postgres, dm/dameng, kingbase, highgo, sqlite, mongodb, redis, elasticsearch)", db.Type)
	}
}

// address 返回驱动应连接的地址；配置了 via_ssh 时启动本地隧道。
func (m *Manager) address(db *config.Database) string {
	if db.ViaSSH == "" {
		return fmt.Sprintf("%s:%d", db.Host, db.Port)
	}
	t := m.getTunnel(db)
	return t.localAddr()
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, d := range m.drv {
		d.Close()
		delete(m.drv, n)
	}
	closeAllTunnels()
}
