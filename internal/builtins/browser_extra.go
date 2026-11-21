package builtins

// browser_extra.go — 对齐官方 @playwright/mcp 的其余浏览器能力：
// 历史后退、下拉选择、文件上传、JS 对话框、拖拽投放、媒体模拟、
// 网络请求详情、任意 JS 执行（unsafe）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// dialogInfo 是一次 JS 对话框（alert/confirm/prompt）的快照。
type dialogInfo struct {
	Type    string
	Message string
	URL     string
}

// dragPoint 是视口内的一点坐标（CSS 像素）。
type dragPoint struct {
	X float64
	Y float64
}

// activeTab 返回当前活动标签页（未启动时先惰性拉起浏览器）。
func (e *browserEngine) activeTab() (*browserTab, error) {
	if err := e.ensure(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active < 0 || e.active >= len(e.tabs) {
		return nil, fmt.Errorf("无活动标签页")
	}
	return e.tabs[e.active], nil
}

// navigateBack 浏览器后退一步；没有更早历史时返回明确错误。
func (e *browserEngine) navigateBack() error {
	err := e.run(chromedp.NavigateBack())
	if err != nil && strings.Contains(err.Error(), "invalid navigation entry") {
		return fmt.Errorf("没有更早的历史记录")
	}
	return err
}

// selectOption 选择下拉框选项，values 可匹配 option 的 value 或可见文本，
// 匹配后派发 input/change 事件让页面框架（Vue/React 等）感知。
func (e *browserEngine) selectOption(ref string, values []string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("ref/selector 不能为空")
	}
	if len(values) == 0 {
		return "", fmt.Errorf("values 不能为空")
	}
	valsJSON, _ := json.Marshal(values)
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'not found';
		if (el.tagName !== 'SELECT') return 'not a select';
		const wanted = %s;
		const matched = [];
		[...el.options].forEach(o => {
			const hit = wanted.includes(o.value) || wanted.includes(o.textContent.trim());
			o.selected = hit;
			if (hit) matched.push(o.value || o.textContent.trim());
		});
		if (matched.length === 0) return 'no match';
		el.dispatchEvent(new Event('input', {bubbles: true}));
		el.dispatchEvent(new Event('change', {bubbles: true}));
		return 'ok: ' + matched.join(', ');
	})()`, ref, string(valsJSON))
	v, err := e.eval(expr)
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	switch s {
	case "not found":
		return "", fmt.Errorf("未找到元素 %s", ref)
	case "not a select":
		return "", fmt.Errorf("元素 %s 不是 <select> 下拉框", ref)
	case "no match":
		return "", fmt.Errorf("没有匹配的选项（可匹配 value 或文本）: %v", values)
	}
	return strings.TrimPrefix(s, "ok: "), nil
}

// uploadFiles 把本地文件挂到 file input（CDP DOM.setFileInputFiles），
// 无需点击触发系统选择框。
func (e *browserEngine) uploadFiles(ref string, paths []string) error {
	if ref == "" {
		return fmt.Errorf("ref/selector 不能为空")
	}
	if len(paths) == 0 {
		return fmt.Errorf("paths 不能为空")
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("文件不存在: %s", p)
		}
		if st.IsDir() {
			return fmt.Errorf("路径是目录不是文件: %s", p)
		}
	}
	var nodes []*cdp.Node
	if err := e.run(chromedp.Nodes(ref, &nodes, chromedp.ByQuery)); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("未找到元素 %s", ref)
	}
	return e.run(dom.SetFileInputFiles(paths).WithNodeID(nodes[0].NodeID))
}

// handleDialog 响应待处理的 JS 对话框。对话框打开期间目标页面被暂停，
// 但 CDP 命令按消息 ID 独立收发，这里仍能送达（即使触发对话框的调用还阻塞着）。
func (e *browserEngine) handleDialog(accept bool, promptText string) (string, error) {
	tab, err := e.activeTab()
	if err != nil {
		return "", err
	}
	tab.mu.Lock()
	pd := tab.pendingDialog
	tab.mu.Unlock()
	if pd == nil {
		return "", fmt.Errorf("没有待处理的对话框（页面触发 alert/confirm/prompt 后这里会出现）")
	}
	ctx, cancel := context.WithTimeout(tab.ctx, 10*time.Second)
	defer cancel()
	params := page.HandleJavaScriptDialog(accept)
	if promptText != "" {
		params = params.WithPromptText(promptText)
	}
	if err := chromedp.Run(ctx, params); err != nil {
		return "", err
	}
	tab.mu.Lock()
	tab.pendingDialog = nil
	tab.mu.Unlock()
	verb := "已接受"
	if !accept {
		verb = "已取消"
	}
	return fmt.Sprintf("%s %s 对话框: %s", verb, pd.Type, pd.Message), nil
}

// resolveCenter 把 ref/selector 解析成视口坐标（先滚动到视野中央再取中心点）。
func (e *browserEngine) resolveCenter(ref string) (dragPoint, error) {
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'not found';
		el.scrollIntoView({block:'center'});
		const r = el.getBoundingClientRect();
		return JSON.stringify({x: r.x + r.width / 2, y: r.y + r.height / 2});
	})()`, ref)
	v, err := e.eval(expr)
	if err != nil {
		return dragPoint{}, err
	}
	s, _ := v.(string)
	if s == "not found" {
		return dragPoint{}, fmt.Errorf("未找到元素 %s", ref)
	}
	var p dragPoint
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return dragPoint{}, fmt.Errorf("解析元素坐标失败: %w", err)
	}
	return p, nil
}

// drag 从起点元素按住左键移动到终点元素。
// release=false 时保持按住（只派发到 mousemove），之后用 browser_drop 投放。
// 基于 JS 合成事件（mousedown/mousemove/mouseup），mousemove 目标用
// document.elementFromPoint 解析，模拟真实事件路由。
// 不走 CDP Input 域：Edge 147 的 CDP 鼠标事件命中测试存在竞态
// （browser_probe_test 多轮验证），合成事件路径确定可用。
// 原生 HTML5 dnd 需要 drag event，暂不支持。
func (e *browserEngine) drag(fromRef, toRef string, steps int, release bool) error {
	if fromRef == "" || toRef == "" {
		return fmt.Errorf("startRef/endRef 不能为空")
	}
	if steps <= 0 {
		steps = 6
	}
	if steps > 50 {
		steps = 50
	}
	expr := fmt.Sprintf(`(() => {
		const s = document.querySelector(%q), d = document.querySelector(%q);
		if (!s || !d) return 'not found';
		s.scrollIntoView({block:'center'}); d.scrollIntoView({block:'center'});
		const rs = s.getBoundingClientRect(), rd = d.getBoundingClientRect();
		const sx = rs.x + rs.width / 2, sy = rs.y + rs.height / 2;
		const dx = rd.x + rd.width / 2, dy = rd.y + rd.height / 2;
		const me = (type, x, y, el) => el.dispatchEvent(new MouseEvent(type, {
			bubbles: true, cancelable: true, view: window, clientX: x, clientY: y, button: 0 }));
		me('mousedown', sx, sy, s);
		me('mousemove', sx, sy, s);
		for (let i = 1; i <= %d; i++) {
			const t = i / %d;
			const x = sx + (dx - sx) * t, y = sy + (dy - sy) * t;
			me('mousemove', x, y, d);
		}
		if (%v) { me('mouseup', dx, dy, d); return 'ok'; }
		return 'holding';
	})()`, fromRef, toRef, steps, steps, release)
	v, err := e.eval(expr)
	if err != nil {
		return err
	}
	s, _ := v.(string)
	switch s {
	case "not found":
		return fmt.Errorf("未找到起点或终点元素")
	case "holding":
		e.mu.Lock()
		e.dragPos = &dragPoint{}
		e.mu.Unlock()
		return nil
	}
	e.mu.Lock()
	e.dragPos = nil
	e.mu.Unlock()
	return nil
}

// drop 把拖拽释放到指定元素上（配合 browser_drag 的 release=false）。
func (e *browserEngine) drop(ref string) error {
	if ref == "" {
		return fmt.Errorf("ref/selector 不能为空")
	}
	e.mu.Lock()
	pos := e.dragPos
	e.mu.Unlock()
	if pos == nil {
		return fmt.Errorf("没有进行中的拖拽（先调 browser_drag 且 release=false）")
	}
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'not found';
		el.scrollIntoView({block:'center'});
		const r = el.getBoundingClientRect();
		const x = r.x + r.width / 2, y = r.y + r.height / 2;
		el.dispatchEvent(new MouseEvent('mousemove', {
			bubbles: true, cancelable: true, view: window, clientX: x, clientY: y, button: 0 }));
		el.dispatchEvent(new MouseEvent('mouseup', {
			bubbles: true, cancelable: true, view: window, clientX: x, clientY: y, button: 0 }));
		return 'ok';
	})()`, ref)
	v, err := e.eval(expr)
	if err != nil {
		return err
	}
	if s, _ := v.(string); s == "not found" {
		return fmt.Errorf("未找到元素 %s", ref)
	}
	e.mu.Lock()
	e.dragPos = nil
	e.mu.Unlock()
	return nil
}

// emulateMedia 模拟媒体环境：media=screen|print，colorScheme=light|dark|...；
// 两项都留空时清空全部媒体模拟。
func (e *browserEngine) emulateMedia(media, colorScheme string) error {
	params := emulation.SetEmulatedMedia()
	switch {
	case media == "" && colorScheme == "":
		params = params.WithFeatures([]*emulation.MediaFeature{})
	case media != "" && colorScheme != "":
		params = params.WithMedia(media).WithFeatures([]*emulation.MediaFeature{
			{Name: "prefers-color-scheme", Value: colorScheme},
		})
	case media != "":
		params = params.WithMedia(media)
	default:
		params = params.WithFeatures([]*emulation.MediaFeature{
			{Name: "prefers-color-scheme", Value: colorScheme},
		})
	}
	return e.run(params)
}

// netDetail 返回单个网络请求详情（响应头 + 响应体，body 截断 20KB）。
func (e *browserEngine) netDetail(reqID string) (string, error) {
	if reqID == "" {
		return "", fmt.Errorf("requestId 不能为空（先调 browser_network_requests 取 rid）")
	}
	tab, err := e.activeTab()
	if err != nil {
		return "", err
	}
	tab.mu.Lock()
	var ent *netEntry
	for i := range tab.network {
		if string(tab.network[i].ReqID) == reqID {
			ent = &tab.network[i]
			break
		}
	}
	if ent == nil {
		tab.mu.Unlock()
		return "", fmt.Errorf("请求 %s 不存在（用 browser_network_requests 查看可用 rid）", reqID)
	}
	detail := *ent // 拷贝后立刻释放锁，取响应体不占用 tab.mu
	tab.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "rid=%s\n%d %s %s\n类型: %s  MIME: %s\n",
		detail.ReqID, detail.Status, detail.Method, detail.URL, detail.Type, detail.MIME)
	if len(detail.Headers) > 0 {
		b.WriteString("-- 响应头 --\n")
		for k, v := range detail.Headers {
			fmt.Fprintf(&b, "%s: %v\n", k, v)
		}
	}
	ctx, cancel := context.WithTimeout(tab.ctx, 15*time.Second)
	defer cancel()
	var body []byte
	// 必须经 chromedp.Run 的 ActionFunc 拿到带 executor 的 ctx，直接 .Do(tab.ctx) 会报 invalid context
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(actx context.Context) error {
		var err error
		body, err = network.GetResponseBody(detail.ReqID).Do(actx) // 返回值已自动 base64 解码
		return err
	}))
	if err != nil {
		fmt.Fprintf(&b, "-- 响应体 --\n(不可用: %v)\n", err)
		return b.String(), nil
	}
	if len(body) > 20000 {
		body = append(body[:20000:20000], []byte("\n...(截断)")...)
	}
	fmt.Fprintf(&b, "-- 响应体 --\n%s\n", body)
	return b.String(), nil
}

// runCode 在页面上下文执行任意 JS（表达式或 IIFE），自动等待 Promise，
// 带命令行 API 与用户手势。对齐官方 browser_run_code_unsafe 的语义：
// 危险能力，仅供可信页面使用。
func (e *browserEngine) runCode(code string) (any, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("code 不能为空")
	}
	tab, err := e.activeTab()
	if err != nil {
		return nil, err
	}
	if tabHasPendingDialog(tab) {
		return nil, fmt.Errorf("页面有待处理的 JS 对话框，先调用 browser_handle_dialog")
	}
	ctx, cancel := context.WithTimeout(tab.ctx, 60*time.Second)
	defer cancel()
	var out any
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		res, expDetails, err := runtime.Evaluate(code).
			WithAwaitPromise(true).
			WithReturnByValue(true).
			WithUserGesture(true).
			Do(ctx)
		if err != nil {
			return err
		}
		if expDetails != nil {
			return fmt.Errorf("JS 异常: %s", expDetails.Exception.Description)
		}
		if res == nil || res.Type == "undefined" {
			out = "undefined"
			return nil
		}
		out = json.RawMessage(res.Value)
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return out, nil
}
