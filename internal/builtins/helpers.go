package builtins

import (
	"encoding/json"
	"fmt"
	"strings"

	"mcp-ops/internal/mcp"
)

// ---------- 参数解析 helper（args 为 map[string]any） ----------

func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argStrings(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func argBool(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func argFloat(args map[string]any, key string) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

// schema 快速构造 JSON Schema（对象型）。
func schema(props string, required ...string) json.RawMessage {
	req := ""
	if len(required) > 0 {
		req = fmt.Sprintf(",\"required\":[%s]", strings.Join(quoteAll(required), ","))
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s}%s}`, props, req))
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// errR 返回一个 isError=true 的结果（工具级错误，回传给模型）。
func errR(format string, a ...any) *mcp.CallToolResult {
	return mcp.ErrResult(fmt.Sprintf(format, a...))
}
