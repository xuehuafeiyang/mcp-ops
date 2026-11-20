package websrv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/temoto/robotstxt"
)

const robotsTTL = time.Hour

type robotsEntry struct {
	data     *robotstxt.RobotsData
	allowAll bool
	expires  time.Time
}

// uaToken 取 User-Agent 的产品 token（"mcp-ops/0.1 (...)" -> "mcp-ops"），
// robots.txt 的 User-agent 匹配按产品 token 语义进行。
func uaToken(ua string) string {
	ua = strings.TrimSpace(ua)
	if i := strings.IndexAny(ua, " /"); i > 0 {
		return ua[:i]
	}
	if ua == "" {
		return "mcp-ops"
	}
	return ua
}

// checkRobots 在发起请求前校验 robots.txt。
// 语义参考 Google 与官方 mcp-server-fetch：
//   - robots.txt 返回 4xx/5xx 或网络错误 -> 视为允许（fail-open，避免站点故障导致完全不可用）
//   - 解析失败 -> 允许
//   - 200 -> 按 User-agent 分组匹配路径
func (f *Fetcher) checkRobots(ctx context.Context, u *url.URL) error {
	wc := f.webCfg()
	if !wc.Robots() {
		return nil
	}
	base := u.Scheme + "://" + u.Host
	entry := f.robotsFor(ctx, base)
	if entry == nil || entry.allowAll || entry.data == nil {
		return nil
	}
	token := uaToken(wc.UA())
	if group := entry.data.FindGroup(token); group != nil && !group.Test(u.RequestURI()) {
		return fmt.Errorf("blocked by robots.txt: %s/robots.txt disallows %q for user-agent %q (set web_fetch.respect_robots: false to override)",
			base, u.RequestURI(), token)
	}
	return nil
}

func (f *Fetcher) robotsFor(ctx context.Context, base string) *robotsEntry {
	f.robotsMu.Lock()
	if e, ok := f.robots[base]; ok && time.Now().Before(e.expires) {
		f.robotsMu.Unlock()
		return e
	}
	f.robotsMu.Unlock()

	entry := f.fetchRobots(ctx, base)

	f.robotsMu.Lock()
	if len(f.robots) > 256 { // 简单容量控制，避免无限增长
		f.robots = make(map[string]*robotsEntry)
	}
	f.robots[base] = entry
	f.robotsMu.Unlock()
	return entry
}

func (f *Fetcher) fetchRobots(ctx context.Context, base string) *robotsEntry {
	allow := func() *robotsEntry {
		return &robotsEntry{allowAll: true, expires: time.Now().Add(robotsTTL)}
	}

	wc := f.webCfg()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/robots.txt", nil)
	if err != nil {
		return allow()
	}
	req.Header.Set("User-Agent", wc.UA())
	for k, v := range wc.DefaultHeaders {
		req.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: f.transport(wc, false),
	}
	resp, err := client.Do(req)
	if err != nil {
		return allow()
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return allow()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return allow()
	}
	data, err := robotstxt.FromBytes(body)
	if err != nil {
		return allow()
	}
	return &robotsEntry{data: data, expires: time.Now().Add(robotsTTL)}
}
