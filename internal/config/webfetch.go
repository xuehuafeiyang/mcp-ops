package config

import "time"

// WebFetchConfig 控制网页抓取行为。所有字段都有安全默认值，省略即用默认。
type WebFetchConfig struct {
	UserAgent      string            `yaml:"user_agent"`       // 默认 mcp-ops/0.1 (...)
	Timeout        Duration          `yaml:"timeout"`          // 默认 30s
	MaxBytes       int64             `yaml:"max_bytes"`        // 响应体上限，默认 10 MiB
	MaxRedirects   int               `yaml:"max_redirects"`    // 默认 5
	RespectRobots  *bool             `yaml:"respect_robots"`   // 默认 true
	AllowPrivate   bool              `yaml:"allow_private"`    // 默认 false：拒绝私网/环回/云元数据地址
	DefaultHeaders map[string]string `yaml:"default_headers"`  // 每次请求附带的头（如 Cookie）
	CacheTTL       *Duration         `yaml:"cache_ttl"`        // 页面缓存，默认 5m；显式写 0s 关闭
	MaxLength      int               `yaml:"max_length"`       // 单次返回字符数，默认 20000
	MaxConcurrent  int               `yaml:"max_concurrent"`   // 并发抓取上限，默认 8
}

// DefaultUserAgent 参考官方 mcp-server-fetch 的做法：UA 明确标识来源与用途，
// 便于站点管理员区分自动化抓取与普通浏览器。
const DefaultUserAgent = "mcp-ops/0.1 (Autonomous Agent; +https://github.com/mcp-ops)"

func (w *WebFetchConfig) UA() string {
	if w == nil || w.UserAgent == "" {
		return DefaultUserAgent
	}
	return w.UserAgent
}

func (w *WebFetchConfig) TimeoutD() time.Duration {
	if w == nil || w.Timeout == 0 {
		return 30 * time.Second
	}
	return w.Timeout.D()
}

func (w *WebFetchConfig) Bytes() int64 {
	if w == nil || w.MaxBytes <= 0 {
		return 10 << 20
	}
	return w.MaxBytes
}

func (w *WebFetchConfig) Redirects() int {
	if w == nil || w.MaxRedirects <= 0 {
		return 5
	}
	return w.MaxRedirects
}

// Robots 默认 true：模型发起的抓取必须遵守 robots.txt（参考官方实现）。
func (w *WebFetchConfig) Robots() bool {
	if w == nil || w.RespectRobots == nil {
		return true
	}
	return *w.RespectRobots
}

func (w *WebFetchConfig) TTL() time.Duration {
	if w == nil || w.CacheTTL == nil {
		return 5 * time.Minute
	}
	return w.CacheTTL.D()
}

func (w *WebFetchConfig) Length() int {
	if w == nil || w.MaxLength <= 0 {
		return 20000
	}
	return w.MaxLength
}

func (w *WebFetchConfig) Concurrency() int {
	if w == nil || w.MaxConcurrent <= 0 {
		return 8
	}
	return w.MaxConcurrent
}
