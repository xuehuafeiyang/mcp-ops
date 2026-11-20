package dbsrv

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mcp-ops/internal/config"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // 金仓/瀚高 复用 PG 协议
	_ "modernc.org/sqlite"             // SQLite 纯 Go 驱动（无 CGO，跨平台）
)

// sqlDriver 覆盖 MySQL / TiDB / PostgreSQL / 达梦 / 金仓 / 瀚高 / SQLite（database/sql 生态）。
// 连接池参数在 DSN 中固定，查询超时用 context 控制。
type sqlDriver struct {
	db  *sql.DB
	typ string
}

// isPGFamily 判断是否 PostgreSQL 协议族（占位符需改写为 $1..$n）。
// 金仓 KingbaseES 与瀚高 HighGo 均基于 PostgreSQL 内核，协议兼容。
func isPGFamily(typ string) bool {
	switch typ {
	case "postgres", "kingbase", "kingbasees", "highgo", "highgodb":
		return true
	}
	return false
}

// isMySQLFamily 判断是否 MySQL 协议族（TiDB 完全兼容 MySQL 协议）。
func isMySQLFamily(typ string) bool {
	return typ == "mysql" || typ == "tidb"
}

func openSQL(typ string, cfg *config.Database, addr string) (*sqlDriver, error) {
	var dsn, driverName string
	switch {
	case isMySQLFamily(typ): // mysql 与 tidb（TiDB 完全兼容 MySQL 协议）
		driverName = "mysql"
		if cfg.DSN != "" && cfg.ViaSSH == "" {
			dsn = cfg.DSN
		} else {
			dsn = fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&timeout=10s&multiStatements=false",
				cfg.User, cfg.Password, addr, cfg.Database)
		}
	case typ == "sqlite" || typ == "sqlite3":
		// SQLite 是文件数据库，无 host/port 概念；DSN 即文件路径（或 :memory:）。
		driverName = "sqlite"
		if cfg.DSN != "" {
			dsn = cfg.DSN
		} else if cfg.Database != "" {
			dsn = cfg.Database
		} else {
			return nil, fmt.Errorf("sqlite 需指定 database 字段为数据库文件路径（如 /data/app.db 或 :memory:），或写 dsn")
		}
	case typ == "dm" || typ == "dameng":
		if !dmAvailable {
			return nil, fmt.Errorf("达梦数据库驱动仅支持 Linux/Windows 平台（当前平台需改用金仓/瀚高或其他库）")
		}
		driverName = "dm"
		if cfg.DSN != "" && cfg.ViaSSH == "" {
			dsn = cfg.DSN
		} else {
			dsn = fmt.Sprintf("dm://%s:%s@%s", cfg.User, cfg.Password, addr)
			if cfg.Database != "" {
				dsn += "?schema=" + url.QueryEscape(cfg.Database)
			}
		}
	case isPGFamily(typ):
		driverName = "pgx"
		if cfg.DSN != "" && cfg.ViaSSH == "" {
			dsn = cfg.DSN
		} else {
			dsn = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&connect_timeout=10",
				url.QueryEscape(cfg.User), url.QueryEscape(cfg.Password), addr, cfg.Database)
		}
	default:
		return nil, fmt.Errorf("openSQL: unsupported type %q", typ)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	if typ == "sqlite" || typ == "sqlite3" {
		// SQLite 文件锁是库级写锁，多连接并发写易触发 "database is locked"。
		// 单连接串行化即可规避，查询超时仍由 context 控制。
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
	}
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqlDriver{db: db, typ: typ}, nil
}

func (d *sqlDriver) Type() string { return d.typ }

func (d *sqlDriver) Close() { _ = d.db.Close() }

// Query 执行 SELECT 类语句，返回 JSON 数组（对象数组，key 为列名）。
// 占位符统一用 ?，PostgreSQL 会自动改写为 $1..$n。
func (d *sqlDriver) Query(ctx context.Context, q string, params []string) (string, error) {
	q = d.prepare(q)
	if q == "" {
		return "", fmt.Errorf("empty query")
	}
	args := toAny(params)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	out := make([]map[string]any, 0, 64)
	vals := make([]any, len(cols))
	for rows.Next() {
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = normalize(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return marshalJSON(out)
}

// Execute 执行写入类语句（INSERT/UPDATE/DELETE/DDL），返回受影响行数等信息。
func (d *sqlDriver) Execute(ctx context.Context, stmt string, params []string) (string, error) {
	stmt = d.prepare(stmt)
	if stmt == "" {
		return "", fmt.Errorf("empty statement")
	}
	res, err := d.db.ExecContext(ctx, stmt, toAny(params)...)
	if err != nil {
		return "", err
	}
	affected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	out := map[string]any{"affected_rows": affected}
	if lastID > 0 {
		out["last_insert_id"] = lastID
	}
	return marshalJSON(out)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// prepare 统一占位符：MySQL/达梦 原生支持 ?，PostgreSQL/金仓/瀚高 需要 $1..$n。
// 转换时跳过单引号/双引号/$$ 块内的问号，避免误改字符串字面量。
func (d *sqlDriver) prepare(q string) string {
	q = strings.TrimSpace(q)
	if !isPGFamily(d.typ) || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	inSingle, inDouble, inDollar := false, false, false
	rs := []rune(q)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case inSingle:
			b.WriteRune(c)
			if c == '\'' && (i+1 >= len(rs) || rs[i+1] != '\'') {
				inSingle = false
			} else if c == '\'' && i+1 < len(rs) && rs[i+1] == '\'' {
				b.WriteRune('\'')
				i++
			}
		case inDouble:
			b.WriteRune(c)
			if c == '"' {
				inDouble = false
			}
		case inDollar:
			b.WriteRune(c)
			if c == '$' && i+1 < len(rs) && rs[i+1] == '$' {
				b.WriteRune('$')
				i++
				inDollar = false
			}
		case c == '\'':
			inSingle = true
			b.WriteRune(c)
		case c == '"':
			inDouble = true
			b.WriteRune(c)
		case c == '$' && i+1 < len(rs) && rs[i+1] == '$':
			inDollar = true
			b.WriteString("$$")
			i++
		case c == '?':
			n++
			fmt.Fprintf(&b, "$%d", n)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// normalize 把 database/sql 扫描出的值转成 JSON 友好类型
func normalize(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case *[]byte:
		if t == nil {
			return nil
		}
		return string(*t)
	default:
		return v
	}
}

func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(b), nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
