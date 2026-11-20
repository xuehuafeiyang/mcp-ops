package dbsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mcp-ops/internal/mcp"
)

// RegisterTools 注册数据库工具到 MCP Server。
// 超时策略：分类可配——每个数据库的 query_timeout（默认 30s），
// 超时或取消返回结构化错误，供 host 端模型自纠。
func RegisterTools(s *mcp.Server, m *Manager) {
	s.RegisterTool("list_databases",
		"列出所有已配置的数据库：名称、类型（mysql/tidb/postgres/dm/kingbase/highgo/sqlite/mongodb/redis/elasticsearch）、是否经 SSH 隧道、查询超时。",
		json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		func(mcp.CallContext, map[string]any) *mcp.CallToolResult {
			dbs := m.cfg.Get().Databases
			var out []map[string]any
			for _, d := range dbs {
				out = append(out, map[string]any{
					"name":            d.Name,
					"type":            d.Type,
					"via_ssh":         d.ViaSSH,
					"query_timeout_seconds": int(d.Timeout().Seconds()),
				})
			}
			if out == nil {
				out = []map[string]any{}
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			return mcp.TextResult(string(b))
		})

	registerQueryTool(s, m, "db_query", false)
	registerQueryTool(s, m, "db_execute", true)
}

func registerQueryTool(s *mcp.Server, m *Manager, name string, isExec bool) {
	var desc, verb string
	if isExec {
		name, verb = "db_execute", "写"
		desc = "在指定数据库上执行写入操作（全读写模型）。" +
			"SQL 库为 INSERT/UPDATE/DELETE/DDL（支持 ? 参数化 params）；" +
			"MongoDB 为写操作 JSON 描述符（insertOne/insertMany/updateOne/updateMany/deleteOne/deleteMany/createIndexes）；" +
			"Redis 为写命令（如 SET key value）；Elasticsearch 为 PUT/POST/DELETE 的 REST 描述符。"
	} else {
		verb = "读"
		desc = "在指定数据库上执行只读查询。" +
			"SQL 库为 SELECT（支持 ? 参数化 params）；" +
			"MongoDB 为读操作 JSON 描述符（find/count/aggregate/distinct/listCollections/dbStats）；" +
			"Redis 为读命令（如 GET key）；Elasticsearch 为 GET/POST 的 REST 描述符（如 _search）。"
	}
	_ = verb

	s.RegisterTool(name, desc,
		json.RawMessage(`{"type":"object","properties":{"database":{"type":"string","description":"数据库名称，先调 list_databases 查看"},"statement":{"type":"string","description":"`+verb+`操作语句或 JSON 描述符"},"params":{"type":"array","items":{"type":"string"},"description":"SQL 库的 ? 占位参数，可选"}},"required":["database","statement"]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			dbName, _ := args["database"].(string)
			stmt, _ := args["statement"].(string)
			var params []string
			if raw, ok := args["params"].([]any); ok {
				for _, p := range raw {
					params = append(params, fmt.Sprintf("%v", p))
				}
			}

			dbCfg := m.cfg.Get().FindDatabase(dbName)
			if dbCfg == nil {
				return mcp.ErrResult(fmt.Sprintf("database %q not found", dbName))
			}
			timeout := dbCfg.Timeout()
			cctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			done := make(chan callRes, 1)
			go func() {
				d, err := m.Get(dbName)
				if err != nil {
					done <- callRes{err: err}
					return
				}
				var out string
				if isExec {
					out, err = d.Execute(cctx, stmt, params)
				} else {
					out, err = d.Query(cctx, stmt, params)
				}
				done <- callRes{out: out, err: err}
			}()

			select {
			case r := <-done:
				if r.err != nil {
					return mcp.ErrResult("DB ERROR: " + r.err.Error())
				}
				return mcp.TextResult(truncateResult(r.out))
			case <-ctx.Cancel:
				cancel()
				return mcp.ErrResult("request cancelled")
			case <-time.After(timeout):
				cancel()
				return mcp.ErrResult(fmt.Sprintf("TIMEOUT: statement exceeded %s; consider adding LIMIT or narrowing the query", timeout))
			}
		})
}

type callRes struct {
	out string
	err error
}

func truncateResult(s string) string {
	const max = 256 * 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... [result truncated, use LIMIT / size to narrow]"
}
