package builtins

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"mcp-ops/internal/config"
	"mcp-ops/internal/mcp"
)

// browserEngine 是 playwright 的底层浏览器引擎（chromedp/CDP）。
// 懒启动一个 Chrome/Chromium 实例，支持多标签页、快照、截图、点击、输入、执行 JS 等。
type browserEngine struct {
	mu     sync.Mutex
	cfg    config.BrowserConfig
	chrome string // 解析后的可执行文件路径

	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc

	tabs    []*browserTab
	active  int
	nextID  int
	started bool
	dragPos *dragPoint // 进行中的拖拽位置（browser_drag release=false 后未释放）
}

type browserTab struct {
	id     int
	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	console       []string
	network       []netEntry
	pendingDialog *dialogInfo // 待处理的 JS 对话框（alert/confirm/prompt）
}

type netEntry struct {
	ReqID   network.RequestID
	Method  string
	URL     string
	Status  int64 // EventLoadingFailed 后为 -1
	Type    string
	MIME    string
	Headers network.Headers // 响应头（browser_network_request 详情用）
}

// RegisterBrowserTools 注册 playwright 浏览器工具（chromedp/CDP 引擎）。
// 原 chrome-devtools 与 playwright 高度重复，已统一为 playwright 一套，
// 其独有能力（网络请求监控）已并入 browser_network_requests。
func RegisterBrowserTools(s *mcp.Server, cfg config.BrowserConfig) {
	eng := newBrowserEngine(cfg)
	RegisterPlaywright(s, eng)
}

func newBrowserEngine(cfg config.BrowserConfig) *browserEngine {
	return &browserEngine{cfg: cfg, active: -1}
}

// ensure 惰性启动浏览器并创建首个标签页。
func (e *browserEngine) ensure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}
	chrome := e.cfg.Chrome()
	if chrome == "" {
		chrome = findChrome()
	}
	if chrome == "" {
		return fmt.Errorf("未找到 Chrome/Chromium 可执行文件，请配置 builtins.browser.executable_path")
	}
	e.chrome = chrome

	// [实验中] 完整复刻官方 @playwright/mcp 的 Edge 启动参数（headed、无禁 GPU）
	var opts []chromedp.ExecAllocatorOption
	opts = append(opts,
		chromedp.ExecPath(chrome),
		// 精简 flags（经 mouseprobe 二分验证）：禁 GPU/强制缩放等会干扰
		// Input 域命中测试的 flag 一律不加
		chromedp.Flag("no-first-run", ""),
		chromedp.Flag("no-default-browser-check", ""),
		chromedp.Flag("disable-infobars", ""),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.WindowSize(1280, 800),
	)
	if dir := e.cfg.ProfileDir(); dir != "" {
		opts = append(opts, chromedp.UserDataDir(dir))
	}
	if e.cfg.IsHeadless() {
		opts = append(opts, chromedp.Headless)
		// headless 下允许软件渲染合成（禁 GPU 且无合成表面时 Input 域
		// 鼠标事件的命中测试会失效——mouseup/mousemove 全落到 body）
		opts = append(opts, chromedp.Flag("enable-unsafe-swiftshader", ""))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	// MO_CDP_DEBUG=1 时输出 CDP 线缆日志（排障用）
	var browserCtx context.Context
	var browserCancel context.CancelFunc
	if os.Getenv("MO_CDP_DEBUG") != "" {
		browserCtx, browserCancel = chromedp.NewContext(allocCtx, chromedp.WithDebugf(
			func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[cdp] "+format+"\n", args...) }))
	} else {
		browserCtx, browserCancel = chromedp.NewContext(allocCtx)
	}

	e.allocCtx, e.allocCancel = allocCtx, allocCancel
	e.browserCtx, e.browserCancel = browserCtx, browserCancel
	e.started = true

	if _, err := e.newTabLocked(); err != nil {
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	return nil
}

func (e *browserEngine) newTabLocked() (*browserTab, error) {
	e.nextID++
	id := e.nextID
	ctx, cancel := chromedp.NewContext(e.browserCtx)
	t := &browserTab{id: id, ctx: ctx, cancel: cancel}
	// 启用网络域并监听控制台/网络/对话框事件
	_ = chromedp.Run(ctx, network.Enable())
	_ = chromedp.Run(ctx, page.Enable())
	chromedp.ListenTarget(ctx, func(ev any) {
		switch evt := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			var parts []string
			for _, a := range evt.Args {
				parts = append(parts, string(a.Value))
			}
			t.mu.Lock()
			t.console = append(t.console, "["+string(evt.Type)+"] "+strings.Join(parts, " "))
			if len(t.console) > 200 {
				t.console = t.console[len(t.console)-200:]
			}
			t.mu.Unlock()
		case *page.EventJavascriptDialogOpening:
			t.mu.Lock()
			t.pendingDialog = &dialogInfo{Type: string(evt.Type), Message: evt.Message, URL: evt.URL}
			t.console = append(t.console, "[dialog] "+string(evt.Type)+": "+evt.Message)
			if len(t.console) > 200 {
				t.console = t.console[len(t.console)-200:]
			}
			t.mu.Unlock()
		case *network.EventRequestWillBeSent:
			t.mu.Lock()
			t.network = append(t.network, netEntry{
				ReqID: evt.RequestID, Method: evt.Request.Method, URL: evt.Request.URL,
			})
			if len(t.network) > 200 {
				t.network = t.network[len(t.network)-200:]
			}
			t.mu.Unlock()
		case *network.EventResponseReceived:
			t.mu.Lock()
			for i := len(t.network) - 1; i >= 0; i-- {
				if t.network[i].ReqID == evt.RequestID {
					t.network[i].Status = evt.Response.Status
					t.network[i].Type = evt.Type.String()
					t.network[i].MIME = evt.Response.MimeType
					t.network[i].Headers = evt.Response.Headers
					break
				}
			}
			t.mu.Unlock()
		case *network.EventLoadingFailed:
			t.mu.Lock()
			for i := len(t.network) - 1; i >= 0; i-- {
				if t.network[i].ReqID == evt.RequestID {
					t.network[i].Status = -1
					break
				}
			}
			t.mu.Unlock()
		}
	})
	e.tabs = append(e.tabs, t)
	e.active = len(e.tabs) - 1
	return t, nil
}

// run 在活动标签页上执行一组 chromedp action，带超时。
func (e *browserEngine) run(actions ...chromedp.Action) error {
	if err := e.ensure(); err != nil {
		return err
	}
	e.mu.Lock()
	idx := e.active
	if idx < 0 || idx >= len(e.tabs) {
		e.mu.Unlock()
		return fmt.Errorf("无活动标签页")
	}
	tab := e.tabs[idx]
	e.mu.Unlock()

	if tabHasPendingDialog(tab) {
		return fmt.Errorf("页面有待处理的 JS 对话框（alert/confirm/prompt），先调用 browser_handle_dialog")
	}
	ctx, cancel := context.WithTimeout(tab.ctx, 60*time.Second)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// eval 在活动标签页执行 JS 并返回结果。
func (e *browserEngine) eval(expr string) (any, error) {
	if err := e.ensure(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	tab := e.tabs[e.active]
	e.mu.Unlock()
	if tabHasPendingDialog(tab) {
		return nil, fmt.Errorf("页面有待处理的 JS 对话框（alert/confirm/prompt），先调用 browser_handle_dialog")
	}
	ctx, cancel := context.WithTimeout(tab.ctx, 30*time.Second)
	defer cancel()
	var out any
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &out)); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- 具体操作 ----------

func (e *browserEngine) navigate(url string) error {
	if url == "" {
		return fmt.Errorf("url 不能为空")
	}
	lower := strings.ToLower(url)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") &&
		!strings.HasPrefix(lower, "data:") && !strings.HasPrefix(lower, "about:") &&
		!strings.HasPrefix(lower, "file:") {
		url = "https://" + url
	}
	return e.run(chromedp.Navigate(url))
}

func (e *browserEngine) title() (string, error) {
	var t string
	if err := e.run(chromedp.Title(&t)); err != nil {
		return "", err
	}
	return t, nil
}

func (e *browserEngine) content() (string, error) {
	var html string
	if err := e.run(chromedp.OuterHTML("html", &html)); err != nil {
		return "", err
	}
	return html, nil
}

// snapshot 返回页面可交互元素摘要，每个元素带 [ref]（CSS selector）。
func (e *browserEngine) snapshot() (string, error) {
	expr := `(() => {
		const els = document.querySelectorAll('a,button,input,select,textarea,[role="button"],[onclick],[contenteditable="true"]');
		const out = [];
		els.forEach((el, i) => {
			if (i > 300) return;
			const tag = el.tagName.toLowerCase();
			let label = '';
			if (tag === 'input') label = el.value || el.placeholder || el.name || '';
			else if (tag === 'select') label = (el.selectedOptions[0] && el.selectedOptions[0].textContent) || '';
			else label = (el.innerText || el.textContent || '').trim();
			label = label.replace(/\s+/g,' ').slice(0,80);
			let ref = el.getAttribute('data-mcp-ref');
			if (!ref) {
				if (el.id) ref = '#' + el.id;
				else if (el.name && tag === 'input') ref = 'input[name="' + el.name + '"]';
				else {
					const path = [];
					let node = el;
					while (node && node !== document.body && path.length < 6) {
						let nth = 1;
						let sib = node.previousElementSibling;
						while (sib) { if (sib.tagName === node.tagName) nth++; sib = sib.previousElementSibling; }
						path.unshift(node.tagName.toLowerCase() + ':nth-of-type(' + nth + ')');
						node = node.parentElement;
					}
					ref = 'body ' + path.join(' > ');
				}
				try { el.setAttribute('data-mcp-ref', ref); } catch(e) {}
			}
			out.push('[ref=' + ref + '] <' + tag + '>' + (label || '(无文本)'));
		});
		return out.join('\n');
	})()`
	v, err := e.eval(expr)
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	if strings.TrimSpace(s) == "" {
		return "(页面无可交互元素)", nil
	}
	return s, nil
}

func (e *browserEngine) screenshot(path string) (string, error) {
	if path == "" {
		path = filepath.Join(".", fmt.Sprintf("screenshot-%d.png", time.Now().Unix()))
	}
	if !strings.HasSuffix(path, ".png") {
		path += ".png"
	}
	var buf []byte
	if err := e.run(chromedp.CaptureScreenshot(&buf)); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return "", fmt.Errorf("保存截图失败: %w", err)
	}
	return path, nil
}

// click 用 JS 合成点击：focus + mousedown/mouseup 合成事件 + el.click()。
// 不走 CDP Input 域——Edge 147 的 CDP 鼠标事件命中测试存在竞态
// （browser_probe_test 多轮验证：元素监听时收时失），合成事件路径确定可用。
// el.click() 产生 trusted click 事件，原生交互（链接跳转、checkbox 翻转）有效。
func (e *browserEngine) click(ref string) error {
	if ref == "" {
		return fmt.Errorf("ref/selector 不能为空")
	}
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'not found';
		el.scrollIntoView({block:'center'});
		const r = el.getBoundingClientRect();
		const x = r.x + r.width / 2, y = r.y + r.height / 2;
		const o = {bubbles: true, cancelable: true, view: window, clientX: x, clientY: y, button: 0};
		if (typeof el.focus === 'function') { try { el.focus(); } catch (e) {} }
		el.dispatchEvent(new MouseEvent('mousedown', o));
		el.dispatchEvent(new MouseEvent('mousemove', o));
		el.dispatchEvent(new MouseEvent('mouseup', o));
		el.click();
		return 'ok';
	})()`, ref)
	v, err := e.eval(expr)
	if err != nil {
		return err
	}
	if s, _ := v.(string); s == "not found" {
		return fmt.Errorf("未找到元素 %s", ref)
	}
	return nil
}

func (e *browserEngine) typeText(ref, text string) error {
	if ref == "" {
		return fmt.Errorf("ref/selector 不能为空")
	}
	return e.run(chromedp.SendKeys(ref, text, chromedp.ByQuery))
}

func (e *browserEngine) pressKey(key string) error {
	if key == "" {
		return fmt.Errorf("key 不能为空")
	}
	return e.run(chromedp.KeyEvent(key))
}

func (e *browserEngine) hover(ref string) error {
	if ref == "" {
		return fmt.Errorf("ref/selector 不能为空")
	}
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'not found';
		el.scrollIntoView({block:'center'});
		el.dispatchEvent(new MouseEvent('mouseover', {bubbles:true}));
		el.dispatchEvent(new MouseEvent('mouseenter', {bubbles:true}));
		return 'ok';
	})()`, ref)
	v, err := e.eval(expr)
	if err != nil {
		return err
	}
	if s, _ := v.(string); s == "not found" {
		return fmt.Errorf("未找到元素 %s", ref)
	}
	return nil
}

func (e *browserEngine) wait(ms int) error {
	if ms <= 0 {
		ms = 1000
	}
	if ms > 60000 {
		ms = 60000
	}
	return e.run(chromedp.Sleep(time.Duration(ms) * time.Millisecond))
}

func (e *browserEngine) resize(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("width 与 height 必须为正")
	}
	return e.run(chromedp.EmulateViewport(int64(w), int64(h)))
}

func (e *browserEngine) console() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active < 0 || e.active >= len(e.tabs) {
		return "(无控制台消息)"
	}
	t := e.tabs[e.active]
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.console) == 0 {
		return "(无控制台消息)"
	}
	return strings.Join(t.console, "\n")
}

func (e *browserEngine) netList() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active < 0 || e.active >= len(e.tabs) {
		return "(无网络请求)"
	}
	t := e.tabs[e.active]
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.network) == 0 {
		return "(无网络请求)"
	}
	var b strings.Builder
	for _, n := range t.network {
		fmt.Fprintf(&b, "rid=%s %d %s %s [%s]\n", n.ReqID, n.Status, n.Method, n.URL, n.Type)
	}
	return b.String()
}

// tabHasPendingDialog 检查标签页是否有未响应的 JS 对话框。
// 对话框打开期间目标页面被暂停，任何 CDP 命令都会被卡住，所以要快失败。
func tabHasPendingDialog(t *browserTab) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingDialog != nil
}

// ---------- 标签页管理 ----------

func (e *browserEngine) newTab() (int, error) {
	if err := e.ensure(); err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.newTabLocked()
	if err != nil {
		return 0, err
	}
	return t.id, nil
}

func (e *browserEngine) listTabs() string {
	if err := e.ensure(); err != nil {
		return "浏览器未启动"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var b strings.Builder
	for i, t := range e.tabs {
		mark := " "
		if i == e.active {
			mark = "*"
		}
		var title string
		ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
		_ = chromedp.Run(ctx, chromedp.Title(&title))
		cancel()
		fmt.Fprintf(&b, "%s tab-%d %s\n", mark, t.id, title)
	}
	return b.String()
}

func (e *browserEngine) selectTab(id int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, t := range e.tabs {
		if t.id == id {
			e.active = i
			return nil
		}
	}
	return fmt.Errorf("标签页 %d 不存在", id)
}

func (e *browserEngine) closeTab(id int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, t := range e.tabs {
		if t.id == id {
			t.cancel()
			e.tabs = append(e.tabs[:i], e.tabs[i+1:]...)
			if len(e.tabs) == 0 {
				e.active = -1
			} else if e.active >= len(e.tabs) {
				e.active = len(e.tabs) - 1
			}
			return nil
		}
	}
	return fmt.Errorf("标签页 %d 不存在", id)
}

func (e *browserEngine) closeAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.tabs {
		t.cancel()
	}
	e.tabs = nil
	e.active = -1
	if e.browserCancel != nil {
		e.browserCancel()
	}
	if e.allocCancel != nil {
		e.allocCancel()
	}
	e.started = false
}

// findChrome 按常见路径探测 Chrome/Chromium。
func findChrome() string {
	candidates := []string{
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/opt/google/chrome/google-chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("google-chrome"); err == nil {
		return p
	}
	if p, err := exec.LookPath("chromium"); err == nil {
		return p
	}
	return ""
}
