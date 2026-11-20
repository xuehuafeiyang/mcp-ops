package websrv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcp-ops/internal/config"
)

// FetchInput 是一次抓取请求的参数（对应工具参数）。
type FetchInput struct {
	URL        string
	MaxLength  int
	StartIndex int
	Raw        bool
}

// FetchResult 是抓取结果。Text 为分页后的片段，TotalChars 为全文长度。
type FetchResult struct {
	RequestURL  string
	FinalURL    string
	Status      int
	ContentType string
	Title       string
	Text        string
	TotalChars  int
	StartIndex  int
	EndIndex    int
	More        bool // 后面还有内容，可用 EndIndex 作为 start_index 续读
	FromCache   bool
}

type cacheEntry struct {
	result  *FetchResult
	expires time.Time
}

// Fetcher 封装网页抓取：SSRF 防护、robots.txt、正文提取、分页与缓存。
type Fetcher struct {
	cfg *config.Manager
	sem chan struct{}

	cacheMu sync.Mutex
	cache   map[string]*cacheEntry

	robotsMu sync.Mutex
	robots   map[string]*robotsEntry
}

func NewFetcher(mgr *config.Manager) *Fetcher {
	concurrency := mgr.Get().WebFetch.Concurrency()
	return &Fetcher{
		cfg:    mgr,
		sem:    make(chan struct{}, concurrency),
		cache:  map[string]*cacheEntry{},
		robots: map[string]*robotsEntry{},
	}
}

func (f *Fetcher) webCfg() *config.WebFetchConfig {
	return &f.cfg.Get().WebFetch
}

// transport 每次请求构造。不读取 HTTP_PROXY 等环境代理：
// 走代理时 Dial 看到的是代理地址，SSRF 校验会失去意义。
func (f *Fetcher) transport(wc *config.WebFetchConfig, _ bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   dialControl(wc.AllowPrivate),
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: wc.TimeoutD(),
		MaxIdleConns:          wc.Concurrency(),
		IdleConnTimeout:       30 * time.Second,
		Proxy:                 nil,
	}
}

// Fetch 抓取一个 URL 并返回 Markdown 文本（或原始内容）。
func (f *Fetcher) Fetch(ctx context.Context, in FetchInput) (*FetchResult, error) {
	wc := f.webCfg()

	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return nil, fmt.Errorf("url is required")
	}
	// 容错：允许裸域名 example.com/path
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if err := checkURL(ctx, u, wc.AllowPrivate); err != nil {
		return nil, err
	}

	maxLen := in.MaxLength
	if maxLen <= 0 {
		maxLen = wc.Length()
	}
	if maxLen > 200000 {
		maxLen = 200000
	}
	start := in.StartIndex
	if start < 0 {
		start = 0
	}

	// 缓存键不含分页参数：缓存的是全文，分页在命中后做
	key := u.String() + "|raw=" + strconv.FormatBool(in.Raw)
	if cached := f.cacheGet(key); cached != nil {
		out := *cached
		out.FromCache = true
		applyPaging(&out, start, maxLen)
		return &out, nil
	}

	// 并发闸门：避免模型一次抓几十个页面把目标站打爆
	select {
	case f.sem <- struct{}{}:
		defer func() { <-f.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := f.checkRobots(ctx, u); err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, wc.TimeoutD())
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", wc.UA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	for k, v := range wc.DefaultHeaders {
		req.Header.Set(k, v)
	}

	tr := f.transport(wc, false)
	defer tr.CloseIdleConnections()

	client := &http.Client{
		Transport: tr,
		Timeout:   wc.TimeoutD(),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= wc.Redirects() {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			// 每一跳都重新做 SSRF 校验与 robots 校验（官方实现的同款做法）
			if err := checkURL(r.Context(), r.URL, wc.AllowPrivate); err != nil {
				return err
			}
			return f.checkRobots(r.Context(), r.URL)
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("fetch %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	limit := wc.Bytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds max_bytes (%s); try a smaller page or raise web_fetch.max_bytes", humanBytes(limit))
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" {
		mediaType = http.DetectContentType(body)
		if mt, _, err := mime.ParseMediaType(mediaType); err == nil {
			mediaType = mt
		}
	}

	if resp.StatusCode >= 400 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return nil, fmt.Errorf("HTTP %d %s (final url: %s)%s", resp.StatusCode, http.StatusText(resp.StatusCode), resp.Request.URL, snippetSuffix(snippet))
	}

	var text, title string
	switch {
	case in.Raw:
		if isHTMLType(mediaType) {
			decoded, derr := decodeHTML(bytes.NewReader(body), contentType)
			if derr != nil {
				decoded = string(body)
			}
			text = decoded
		} else {
			text = string(body)
		}
	case isHTMLType(mediaType):
		decoded, derr := decodeHTML(bytes.NewReader(body), contentType)
		if derr != nil {
			decoded = string(body)
		}
		var xerr error
		title, text, xerr = extractArticle(decoded, resp.Request.URL)
		if xerr != nil {
			return nil, xerr
		}
	case isTextualType(mediaType):
		text = string(body)
	default:
		return nil, fmt.Errorf("unsupported content type %q: only HTML, text/*, JSON and XML can be read; binary files (images, PDFs, archives) are not supported", contentType)
	}

	res := &FetchResult{
		RequestURL:  u.String(),
		FinalURL:    resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: contentType,
		Title:       title,
		Text:        text,
		TotalChars:  len([]rune(text)),
	}
	f.cachePut(key, res)

	out := *res
	applyPaging(&out, start, maxLen)
	return &out, nil
}

func isHTMLType(mediaType string) bool {
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

func isTextualType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	// 图片/音视频等一律按二进制拒绝，唯独 SVG 是文本格式，读源码有意义
	if strings.HasPrefix(mediaType, "image/") {
		return mediaType == "image/svg+xml"
	}
	switch mediaType {
	case "application/json", "application/xml", "application/javascript",
		"application/x-yaml", "application/yaml", "application/rss+xml",
		"application/atom+xml", "application/ld+json":
		return true
	}
	return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
}

// applyPaging 按 rune 切分，避免切断多字节字符产生乱码。
func applyPaging(res *FetchResult, start, maxLen int) {
	runes := []rune(res.Text)
	total := len(runes)
	if start > total {
		start = total
	}
	end := start + maxLen
	if end > total {
		end = total
	}
	res.StartIndex = start
	res.EndIndex = end
	res.TotalChars = total
	res.Text = string(runes[start:end])
	res.More = end < total
}

func (f *Fetcher) cacheGet(key string) *FetchResult {
	wc := f.webCfg()
	if wc.TTL() <= 0 {
		return nil
	}
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()
	e, ok := f.cache[key]
	if !ok || time.Now().After(e.expires) {
		delete(f.cache, key)
		return nil
	}
	return e.result
}

func (f *Fetcher) cachePut(key string, res *FetchResult) {
	wc := f.webCfg()
	ttl := wc.TTL()
	if ttl <= 0 {
		return
	}
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()
	if len(f.cache) > 128 {
		f.cache = make(map[string]*cacheEntry)
	}
	f.cache[key] = &cacheEntry{result: res, expires: time.Now().Add(ttl)}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func snippetSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}
