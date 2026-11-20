package websrv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"mcp-ops/internal/config"
)

func newFetcher(t *testing.T, allowPrivate bool, extra string) *Fetcher {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("web_fetch:\n  allow_private: %v\n  cache_ttl: 0s\n%s", allowPrivate, extra)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := config.LoadAndWatch(p)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return NewFetcher(m)
}

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // 云元数据
		{"100.64.0.1", true},      // CGNAT
		{"0.0.0.0", true},
		{"::1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2001:4860:4860::8888", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := isBlockedIP(ip); got != c.blocked {
			t.Errorf("isBlockedIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}

func TestSSRFBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>should not reach</body></html>")
	}))
	defer srv.Close()

	f := newFetcher(t, false, "") // allow_private: false
	_, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err == nil {
		t.Fatal("expected SSRF rejection for loopback, got nil")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("want SSRF error, got: %v", err)
	}
}

func TestFetchHTMLExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>测试文章标题</title>
<script>var evil = "should be removed";</script>
<style>.x{color:red}</style></head>
<body><nav>导航栏噪音</nav>
<article><h1>测试文章标题</h1>
<p>这是正文第一段，用于验证正文提取是否正常工作。</p>
<p>这是正文第二段，包含足够多的文字以便 Readability 判定为文章主体内容。</p></article>
<footer>页脚噪音</footer></body></html>`)
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	res, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(res.Text, "正文第一段") {
		t.Errorf("missing article body, got: %s", res.Text)
	}
	if strings.Contains(res.Text, "should be removed") {
		t.Errorf("script content leaked into output")
	}
	if res.TotalChars == 0 {
		t.Errorf("TotalChars should be > 0")
	}
	if res.FinalURL != srv.URL {
		t.Errorf("FinalURL = %q, want %q", res.FinalURL, srv.URL)
	}
}

func TestPagination(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "第%d段正文。", i)
	}
	long := b.String() // 非周期性文本，避免切片内容恰好相同
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><article><p>%s</p></article></body></html>", long)
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	first, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL, MaxLength: 100})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !first.More {
		t.Fatalf("expected More=true for truncated content (total=%d, got=%d)", first.TotalChars, len([]rune(first.Text)))
	}
	if len([]rune(first.Text)) > 100 {
		t.Errorf("chunk length %d exceeds max_length 100", len([]rune(first.Text)))
	}

	second, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL, MaxLength: 100, StartIndex: first.EndIndex})
	if err != nil {
		t.Fatalf("fetch second chunk: %v", err)
	}
	if second.StartIndex != first.EndIndex {
		t.Errorf("second.StartIndex = %d, want %d", second.StartIndex, first.EndIndex)
	}
	if second.Text == first.Text {
		t.Errorf("second chunk identical to first; pagination not applied")
	}
}

func TestRobotsDisallow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
	})
	mux.HandleFunc("/private/secret", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>secret</body></html>")
	})
	mux.HandleFunc("/public", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p>public page</p></body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newFetcher(t, true, "")
	_, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL + "/private/secret"})
	if err == nil || !strings.Contains(err.Error(), "robots.txt") {
		t.Fatalf("want robots.txt rejection, got: %v", err)
	}

	if _, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL + "/public"}); err != nil {
		t.Fatalf("public page should be allowed, got: %v", err)
	}
}

func TestRobotsDisabled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p>content</p></body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newFetcher(t, true, "  respect_robots: false\n")
	if _, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL + "/page"}); err != nil {
		t.Fatalf("respect_robots=false should bypass robots, got: %v", err)
	}
}

func TestJSONPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"items":[1,2,3]}`)
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	res, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch json: %v", err)
	}
	if !strings.Contains(res.Text, `"ok":true`) {
		t.Errorf("json body not preserved: %s", res.Text)
	}
}

func TestBinaryRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 0x50, 0x4e, 0x47})
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	_, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("want unsupported content type error, got: %v", err)
	}
}

func TestMaxBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("a", 2048)))
	}))
	defer srv.Close()

	f := newFetcher(t, true, "  max_bytes: 1024\n")
	_, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("want max_bytes error, got: %v", err)
	}
}

func TestRedirectTracking(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><p>redirected content</p></body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newFetcher(t, true, "")
	res, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL + "/old"})
	if err != nil {
		t.Fatalf("fetch redirect: %v", err)
	}
	if !strings.HasSuffix(res.FinalURL, "/new") {
		t.Errorf("FinalURL = %q, want suffix /new", res.FinalURL)
	}
	if !strings.Contains(res.Text, "redirected content") {
		t.Errorf("missing redirected content")
	}
}

func TestCacheHit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" { // robots 探测不计入页面请求数
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><p>cached page</p></body></html>")
	}))
	defer srv.Close()

	// 该用例需要开启缓存，单独构造配置（helper 默认关闭缓存）
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("web_fetch:\n  allow_private: true\n  cache_ttl: 5m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := config.LoadAndWatch(p)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	f := NewFetcher(m)

	for i := 0; i < 2; i++ {
		if _, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL}); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times, want 1 (second call should be cached)", got)
	}
}

func TestGBKDecoding(t *testing.T) {
	htmlSrc := `<html><head><title>中文标题</title></head><body><p>这是 GBK 编码的中文内容，用于验证字符集自动识别。</p></body></html>`
	gbk, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(htmlSrc))
	if err != nil {
		t.Fatalf("encode gbk: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		w.Write(gbk)
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	res, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch gbk: %v", err)
	}
	if !strings.Contains(res.Text, "GBK 编码的中文内容") {
		t.Errorf("GBK content not decoded correctly, got: %s", res.Text)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found here", http.StatusNotFound)
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	_, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got: %v", err)
	}
}

func TestRawMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><h1>Raw</h1><script>x</script></body></html>`)
	}))
	defer srv.Close()

	f := newFetcher(t, true, "")
	res, err := f.Fetch(context.Background(), FetchInput{URL: srv.URL, Raw: true})
	if err != nil {
		t.Fatalf("fetch raw: %v", err)
	}
	if !strings.Contains(res.Text, "<h1>Raw</h1>") {
		t.Errorf("raw mode should preserve HTML tags, got: %s", res.Text)
	}
}
