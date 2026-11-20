package postmansrv

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"mcp-ops/internal/config"
	"mcp-ops/internal/mcp"
)

// RegisterTools 注册 Postman 测试工具到 MCP Server。
func RegisterTools(s *mcp.Server, mgr *config.Manager) {
	eng := NewEngine(&mgr.Get().Postman)

	s.RegisterTool("postman_list_collections",
		"列出已配置的 Postman collection 文件（名称 + 路径）。",
		json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			cols := mgr.Get().Postman.Collections
			out := make([]map[string]string, 0, len(cols))
			for _, c := range cols {
				out = append(out, map[string]string{"name": c.Name, "path": c.Path})
			}
			return mcp.TextResult(mustJSON(out))
		})

	s.RegisterTool("postman_list_environments",
		"列出已配置的 Postman environment 文件（名称 + 路径）。",
		json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			envs := mgr.Get().Postman.Environments
			out := make([]map[string]string, 0, len(envs))
			for _, e := range envs {
				out = append(out, map[string]string{"name": e.Name, "path": e.Path})
			}
			return mcp.TextResult(mustJSON(out))
		})

	s.RegisterTool("postman_list_requests",
		"列出某个 collection 中的所有请求（完整路径名、方法、URL）。",
		json.RawMessage(`{"type":"object","properties":{
			"collection":{"type":"string","description":"collection 名称或 JSON 文件路径"},
			"environment":{"type":"string","description":"可选，environment 名称或文件路径，用于变量替换"}
		},"required":["collection"]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			coll, err := resolveCollection(&mgr.Get().Postman, argStr(args, "collection"))
			if err != nil {
				return mcp.ErrResult("POSTMAN ERROR: " + err.Error())
			}
			env, _ := resolveEnvironment(&mgr.Get().Postman, argStr(args, "environment"))
			vars := NewVarStore(coll, env)
			var out []map[string]string
			for _, ni := range coll.Flatten() {
				spec, err := buildSpec(ni.Item, vars)
				if err != nil {
					continue
				}
				out = append(out, map[string]string{"name": ni.Name, "method": spec.Method, "url": spec.URL})
			}
			return mcp.TextResult(mustJSON(out))
		})

	s.RegisterTool("postman_run_request",
		"运行 collection 中的单个请求并执行其 test script，返回响应与断言结果。",
		json.RawMessage(`{"type":"object","properties":{
			"collection":{"type":"string","description":"collection 名称或 JSON 文件路径"},
			"request":{"type":"string","description":"请求名称（完整路径名）或序号（从 1 开始）"},
			"environment":{"type":"string","description":"可选，environment 名称或文件路径"}
		},"required":["collection","request"]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			coll, err := resolveCollection(&mgr.Get().Postman, argStr(args, "collection"))
			if err != nil {
				return mcp.ErrResult("POSTMAN ERROR: " + err.Error())
			}
			env, _ := resolveEnvironment(&mgr.Get().Postman, argStr(args, "environment"))
			ni, err := findRequest(coll, argStr(args, "request"))
			if err != nil {
				return mcp.ErrResult("POSTMAN ERROR: " + err.Error())
			}
			vars := NewVarStore(coll, env)
			res := eng.RunItem(ctxCancel(ctx), ni, vars)
			return mcp.TextResult(mustJSON(res))
		})

	s.RegisterTool("postman_run_collection",
		"运行整个 collection 的全部请求并执行各自 test script，返回通过/失败统计与逐请求结果。",
		json.RawMessage(`{"type":"object","properties":{
			"collection":{"type":"string","description":"collection 名称或 JSON 文件路径"},
			"environment":{"type":"string","description":"可选，environment 名称或文件路径"}
		},"required":["collection"]}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			coll, err := resolveCollection(&mgr.Get().Postman, argStr(args, "collection"))
			if err != nil {
				return mcp.ErrResult("POSTMAN ERROR: " + err.Error())
			}
			env, _ := resolveEnvironment(&mgr.Get().Postman, argStr(args, "environment"))
			res := eng.RunCollection(ctxCancel(ctx), coll, env)
			return mcp.TextResult(mustJSON(res))
		})
}

// resolveCollection 按名称或文件路径解析 collection。
func resolveCollection(cfg *config.PostmanConfig, ref string) (*Collection, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("collection 不能为空")
	}
	for _, c := range cfg.Collections {
		if c.Name == ref {
			return LoadCollection(c.Path)
		}
	}
	return LoadCollection(ref)
}

// resolveEnvironment 按名称或文件路径解析 environment；ref 为空返回 nil。
func resolveEnvironment(cfg *config.PostmanConfig, ref string) (*Environment, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	for _, e := range cfg.Environments {
		if e.Name == ref {
			return LoadEnvironment(e.Path)
		}
	}
	return LoadEnvironment(ref)
}

// findRequest 按名称或序号定位 collection 中的请求。
func findRequest(coll *Collection, ref string) (namedItem, error) {
	items := coll.Flatten()
	if n, err := strconv.Atoi(strings.TrimSpace(ref)); err == nil {
		if n >= 1 && n <= len(items) {
			return items[n-1], nil
		}
		return namedItem{}, fmt.Errorf("请求序号 %d 越界（共 %d 个请求）", n, len(items))
	}
	for _, ni := range items {
		if ni.Name == ref {
			return ni, nil
		}
	}
	for _, ni := range items {
		if strings.Contains(ni.Name, ref) {
			return ni, nil
		}
	}
	return namedItem{}, fmt.Errorf("未找到请求 %q", ref)
}

func argStr(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return s
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ctxCancel 桥接 MCP 取消信号到 context。
func ctxCancel(ctx mcp.CallContext) context.Context {
	cctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ctx.Cancel:
			cancel()
		case <-cctx.Done():
		}
	}()
	return cctx
}
