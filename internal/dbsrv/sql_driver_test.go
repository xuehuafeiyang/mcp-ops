package dbsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mcp-ops/internal/config"
)

// TestSQLiteDriver 验证 SQLite 内存库的打开、建表、写入、查询与参数化。
func TestSQLiteDriver(t *testing.T) {
	cfg := &config.Database{Name: "mem", Type: "sqlite", Database: ":memory:"}
	d, err := openSQL("sqlite", cfg, ":0")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := d.Execute(ctx, "CREATE TABLE users(id INTEGER PRIMARY KEY, name TEXT, age INTEGER)", nil); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := d.Execute(ctx, "INSERT INTO users(name, age) VALUES(?, ?), (?, ?)", []string{"alice", "30", "bob", "25"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, err := d.Query(ctx, "SELECT name, age FROM users WHERE age >= ? ORDER BY age", []string{"26"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal result %q: %v", out, err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 行，实际 %d：%s", len(rows), out)
	}
	if rows[0]["name"] != "alice" {
		t.Fatalf("期望 alice，实际 %v", rows[0]["name"])
	}
	// SQLite 数字默认扫描为 int64，normalize 应保留数值类型
	if rows[0]["age"] != float64(30) && rows[0]["age"] != int64(30) {
		t.Fatalf("age 类型异常：%T %v", rows[0]["age"], rows[0]["age"])
	}
}

// TestSQLiteRequiresPath 验证 sqlite 未指定文件路径时返回友好错误。
func TestSQLiteRequiresPath(t *testing.T) {
	cfg := &config.Database{Name: "x", Type: "sqlite"}
	if _, err := openSQL("sqlite", cfg, ":0"); err == nil || !strings.Contains(err.Error(), "文件路径") {
		t.Fatalf("期望「需指定文件路径」错误，实际 %v", err)
	}
}

// TestDBFamily 验证 MySQL/TiDB 与 PG 家族的归属判断。
func TestDBFamily(t *testing.T) {
	if !isMySQLFamily("mysql") || !isMySQLFamily("tidb") {
		t.Fatal("mysql/tidb 应属于 MySQL 协议族")
	}
	if isMySQLFamily("postgres") || isMySQLFamily("sqlite") {
		t.Fatal("postgres/sqlite 不应属于 MySQL 协议族")
	}
	if !isPGFamily("postgres") || !isPGFamily("kingbase") || !isPGFamily("highgo") {
		t.Fatal("postgres/kingbase/highgo 应属于 PG 协议族")
	}
	if isPGFamily("tidb") || isPGFamily("sqlite") {
		t.Fatal("tidb/sqlite 不应属于 PG 协议族")
	}
}

// TestPreparePlaceholder 验证占位符改写仅作用于 PG 家族，且跳过字符串字面量。
func TestPreparePlaceholder(t *testing.T) {
	pg := &sqlDriver{typ: "postgres"}
	if got := pg.prepare("SELECT * FROM t WHERE a = ? AND b = ?"); got != "SELECT * FROM t WHERE a = $1 AND b = $2" {
		t.Fatalf("PG 改写错误：%q", got)
	}
	// 字符串字面量内的 ? 不改写
	if got := pg.prepare("SELECT 'a?b' FROM t WHERE c = ?"); got != "SELECT 'a?b' FROM t WHERE c = $1" {
		t.Fatalf("字面量改写错误：%q", got)
	}

	my := &sqlDriver{typ: "tidb"}
	if got := my.prepare("SELECT * FROM t WHERE a = ?"); got != "SELECT * FROM t WHERE a = ?" {
		t.Fatalf("TiDB 不应改写占位符：%q", got)
	}
}
