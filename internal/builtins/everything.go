package builtins

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"mcp-ops/internal/mcp"
)

// RegisterEverything 注册官方 server-everything 的测试工具子集。
// 该 server 用于验证 MCP 客户端协议实现的完整度；这里提供可在纯 text
// 传输下工作的工具。依赖 sampling / 图片内容的工具会明确返回不支持说明。
func RegisterEverything(s *mcp.Server) {
	s.RegisterTool("echo",
		"原样返回输入的消息（协议连通性测试）。",
		schema(`"message":{"type":"string"}`, "message"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult(argString(args, "message"))
		})

	s.RegisterTool("add",
		"返回两个数字之和。",
		schema(`"a":{"type":"number"},"b":{"type":"number"}`, "a", "b"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult(fmt.Sprintf("%g", argFloat(args, "a")+argFloat(args, "b")))
		})

	s.RegisterTool("printEnv",
		"返回当前进程的环境变量（键值对，按名排序）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			env := os.Environ()
			sort.Strings(env)
			return mcp.TextResult(strings.Join(env, "\n"))
		})

	s.RegisterTool("longRunningOperation",
		"模拟一个耗时操作（按秒睡眠）后返回完成信息。",
		schema(`"duration":{"type":"integer","description":"耗时秒数，默认 1，上限 30"},
			"steps":{"type":"integer","description":"步骤数（仅展示用）"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			d := argInt(args, "duration")
			if d <= 0 {
				d = 1
			}
			if d > 30 {
				d = 30
			}
			steps := argInt(args, "steps")
			if steps <= 0 {
				steps = 1
			}
			per := time.Duration(d) * time.Second / time.Duration(steps)
			for i := 1; i <= steps; i++ {
				select {
				case <-ctx.Cancel:
					return mcp.TextResult("已取消")
				case <-time.After(per):
				}
			}
			return mcp.TextResult(fmt.Sprintf("完成：耗时 %ds，共 %d 步", d, steps))
		})

	s.RegisterTool("getComplexObject",
		"返回一个嵌套的复杂 JSON 对象（结构化输出测试）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			obj := map[string]any{
				"string":  "hello",
				"number":  42,
				"boolean": true,
				"array":   []any{1, "two", 3.0},
				"object":  map[string]any{"nested": "value", "list": []any{"a", "b"}},
			}
			data, _ := json.MarshalIndent(obj, "", "  ")
			return mcp.TextResult(string(data))
		})

	s.RegisterTool("annotatedMessage",
		"返回带元信息说明的消息（注解能力提示）。",
		schema(`"messageType":{"type":"string","description":"如 info/warning/error"},"includeImage":{"type":"boolean"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			t := argString(args, "messageType")
			if t == "" {
				t = "info"
			}
			msg := fmt.Sprintf("[%s] 这是一条带注解的测试消息", t)
			if argBool(args, "includeImage") {
				msg += "\n(注：当前传输仅支持 text 内容，图片内容未包含)"
			}
			return mcp.TextResult(msg)
		})

	s.RegisterTool("sampleLLM",
		"请求客户端用其 LLM 生成文本（sampling 能力测试）。",
		schema(`"prompt":{"type":"string"},"maxTokens":{"type":"integer"}`, "prompt"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return errR("当前客户端未实现 MCP sampling 能力（sampling/createMessage），无法代为调用 LLM。")
		})

	s.RegisterTool("getTinyImage",
		"返回一张小图片（图片内容测试）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return errR("当前传输仅支持 text 内容，无法返回 image 内容块。")
		})
}
