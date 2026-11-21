package builtins

import (
	"encoding/json"
	"fmt"
	"strings"

	"mcp-ops/internal/mcp"
)

// RegisterPlaywright 注册 @playwright/mcp 的常用工具子集（底层 chromedp/CDP）。
func RegisterPlaywright(s *mcp.Server, e *browserEngine) {
	s.RegisterTool("browser_navigate",
		"导航到指定 URL。省略协议时按 https 处理。",
		schema(`"url":{"type":"string"}`, "url"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.navigate(argString(args, "url")); err != nil {
				return errR("%v", err)
			}
			t, _ := e.title()
			return mcp.TextResult(fmt.Sprintf("已导航，当前标题: %s", t))
		})

	s.RegisterTool("browser_snapshot",
		"返回当前页面可交互元素的快照，每个元素带 [ref=...] 引用，供 click/type/hover 使用。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			snap, err := e.snapshot()
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(snap)
		})

	s.RegisterTool("browser_take_screenshot",
		"对当前页面截图并保存到 PNG 文件，返回文件路径。",
		schema(`"filename":{"type":"string","description":"保存路径，默认 screenshot-<时间戳>.png"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p, err := e.screenshot(argString(args, "filename"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("截图已保存: " + p)
		})

	s.RegisterTool("browser_click",
		"点击页面元素（用 browser_snapshot 返回的 ref）。",
		schema(`"ref":{"type":"string","description":"元素引用（snapshot 中的 ref 或 CSS selector）"}`, "ref"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.click(argString(args, "ref")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("已点击")
		})

	s.RegisterTool("browser_type",
		"向元素输入文本（用 ref 定位）。",
		schema(`"ref":{"type":"string"},"text":{"type":"string"}`, "ref", "text"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.typeText(argString(args, "ref"), argString(args, "text")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("已输入")
		})

	s.RegisterTool("browser_fill_form",
		"批量填写表单字段（fields 为 [{ref, value}] 数组）。",
		schema(`"fields":{"type":"array","items":{"type":"object","properties":{"ref":{"type":"string"},"value":{"type":"string"}},"required":["ref","value"]}}`, "fields"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			var filled int
			switch v := args["fields"].(type) {
			case []any:
				for _, f := range v {
					if m, ok := f.(map[string]any); ok {
						if err := e.typeText(argString(m, "ref"), argString(m, "value")); err == nil {
							filled++
						}
					}
				}
			}
			return mcp.TextResult(fmt.Sprintf("已填写 %d 个字段", filled))
		})

	s.RegisterTool("browser_evaluate",
		"在页面上下文执行 JavaScript 并返回结果。",
		schema(`"function":{"type":"string","description":"要执行的 JS 表达式，如 document.title"}`, "function"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			expr := argString(args, "function")
			if expr == "" {
				return errR("function 不能为空")
			}
			v, err := e.eval(expr)
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(fmt.Sprintf("%v", v))
		})

	s.RegisterTool("browser_press_key",
		"发送按键（如 Enter、Escape、Tab、ArrowDown 等）。",
		schema(`"key":{"type":"string"}`, "key"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.pressKey(argString(args, "key")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("已按键")
		})

	s.RegisterTool("browser_hover",
		"悬停到指定元素（用 ref 定位）。",
		schema(`"ref":{"type":"string"}`, "ref"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.hover(argString(args, "ref")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("已悬停")
		})

	s.RegisterTool("browser_wait_for",
		"等待指定毫秒（默认 1000，上限 60000）。",
		schema(`"time":{"type":"integer","description":"毫秒"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.wait(argInt(args, "time")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("等待完成")
		})

	s.RegisterTool("browser_resize",
		"调整视口大小。",
		schema(`"width":{"type":"integer"},"height":{"type":"integer"}`, "width", "height"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.resize(argInt(args, "width"), argInt(args, "height")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("视口已调整")
		})

	s.RegisterTool("browser_console_messages",
		"返回当前标签页收集到的控制台消息。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult(e.console())
		})

	s.RegisterTool("browser_network_requests",
		"返回当前标签页收集到的网络请求（方法、URL、状态码、资源类型）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult(e.netList())
		})

	s.RegisterTool("browser_tabs",
		"标签页管理：action 为 list/new/select/close，配合 tabId 使用。",
		schema(`"action":{"type":"string","description":"list | new | select | close"},
			"tabId":{"type":"integer","description":"select/close 时的标签页 id"}`, "action"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			switch strings.ToLower(argString(args, "action")) {
			case "new":
				id, err := e.newTab()
				if err != nil {
					return errR("%v", err)
				}
				return mcp.TextResult(fmt.Sprintf("已新建标签页 tab-%d", id))
			case "select":
				if err := e.selectTab(argInt(args, "tabId")); err != nil {
					return errR("%v", err)
				}
				return mcp.TextResult("已切换标签页")
			case "close":
				if err := e.closeTab(argInt(args, "tabId")); err != nil {
					return errR("%v", err)
				}
				return mcp.TextResult("已关闭标签页")
			default:
				return mcp.TextResult(e.listTabs())
			}
		})

	s.RegisterTool("browser_close",
		"关闭浏览器（释放进程与所有标签页）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			e.closeAll()
			return mcp.TextResult("浏览器已关闭")
		})

	s.RegisterTool("browser_navigate_back",
		"浏览器后退一步（没有更早历史时报错）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.navigateBack(); err != nil {
				return errR("%v", err)
			}
			t, _ := e.title()
			return mcp.TextResult("已后退，当前标题: " + t)
		})

	s.RegisterTool("browser_select_option",
		"选择下拉框选项（values 匹配 option 的 value 或可见文本，派发 change 事件）。",
		schema(`"ref":{"type":"string","description":"<select> 的 ref 或 CSS selector"},
			"values":{"type":"array","items":{"type":"string"}}`, "ref", "values"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			picked, err := e.selectOption(argString(args, "ref"), argStrings(args, "values"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("已选中: " + picked)
		})

	s.RegisterTool("browser_file_upload",
		"把本地文件设置到 file input（CDP 直挂，无需点击弹出系统选择框）。paths 必须是绝对路径。",
		schema(`"ref":{"type":"string","description":"input[type=file] 的 ref 或 CSS selector"},
			"paths":{"type":"array","items":{"type":"string"}}`, "ref", "paths"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.uploadFiles(argString(args, "ref"), argStrings(args, "paths")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(fmt.Sprintf("已挂载 %d 个文件", len(argStrings(args, "paths"))))
		})

	s.RegisterTool("browser_handle_dialog",
		"处理页面 JS 对话框（alert/confirm/prompt）：accept=true 确认，promptText 用于 prompt 输入。",
		schema(`"accept":{"type":"boolean"},"promptText":{"type":"string"}`, "accept"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			msg, err := e.handleDialog(argBool(args, "accept"), argString(args, "promptText"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(msg)
		})

	s.RegisterTool("browser_drag",
		"从起点元素按住左键拖到终点元素（mouse 事件模拟）。release=false 不释放，之后用 browser_drop 投放。",
		schema(`"startRef":{"type":"string"},"endRef":{"type":"string"},
			"steps":{"type":"integer","description":"插值步数，默认 6，上限 50"},
			"release":{"type":"boolean","description":"默认 true"}`, "startRef", "endRef"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			release := true
			if v, ok := args["release"].(bool); ok {
				release = v
			}
			if err := e.drag(argString(args, "startRef"), argString(args, "endRef"),
				argInt(args, "steps"), release); err != nil {
				return errR("%v", err)
			}
			if release {
				return mcp.TextResult("已拖拽并释放")
			}
			return mcp.TextResult("拖拽进行中（未释放），用 browser_drop 投放到目标元素")
		})

	s.RegisterTool("browser_drop",
		"把进行中的拖拽移动到指定元素并释放（配合 browser_drag 的 release=false）。",
		schema(`"ref":{"type":"string"}`, "ref"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			if err := e.drop(argString(args, "ref")); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult("已释放拖拽")
		})

	s.RegisterTool("browser_emulate_media",
		"模拟媒体环境：media=screen|print，colorScheme=light|dark 等；两项都留空则重置全部模拟。",
		schema(`"media":{"type":"string"},"colorScheme":{"type":"string"}`),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			media, cs := argString(args, "media"), argString(args, "colorScheme")
			if err := e.emulateMedia(media, cs); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(fmt.Sprintf("媒体模拟已设置 (media=%q colorScheme=%q)", media, cs))
		})

	s.RegisterTool("browser_network_request",
		"查看单个网络请求详情（响应头 + 响应体，body 截断 20KB）。requestId 来自 browser_network_requests 输出的 rid= 前缀。",
		schema(`"requestId":{"type":"string"}`, "requestId"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			d, err := e.netDetail(argString(args, "requestId"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(d)
		})

	s.RegisterTool("browser_run_code_unsafe",
		"在页面上下文执行任意 JS（表达式或 IIFE，自动等待 Promise，带命令行 API 与用户手势）。危险能力，仅供可信页面使用。",
		schema(`"code":{"type":"string"}`, "code"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			v, err := e.runCode(argString(args, "code"))
			if err != nil {
				return errR("%v", err)
			}
			if b, ok := v.(json.RawMessage); ok {
				return mcp.TextResult(string(b))
			}
			return mcp.TextResult(fmt.Sprintf("%v", v))
		})
}
