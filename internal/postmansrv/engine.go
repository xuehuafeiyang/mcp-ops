package postmansrv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mcp-ops/internal/config"
)

// Engine 是本地 Postman 测试引擎：执行请求、运行断言脚本。
type Engine struct {
	timeout time.Duration
	client  *http.Client
}

// NewEngine 从配置构造引擎。
func NewEngine(cfg *config.PostmanConfig) *Engine {
	t := cfg.TimeoutD()
	return &Engine{
		timeout: t,
		client:  &http.Client{Timeout: t},
	}
}

// namedItem 携带完整路径名（文件夹/子文件夹 + 请求名）。
type namedItem struct {
	Name string
	Item Item
}

// Flatten 把 collection 的 item 树拍平成请求列表（带完整路径名）。
func (c *Collection) Flatten() []namedItem {
	var out []namedItem
	var walk func(prefix string, items []Item)
	walk = func(prefix string, items []Item) {
		for _, it := range items {
			name := it.Name
			if prefix != "" {
				name = prefix + " / " + it.Name
			}
			if it.IsFolder() {
				walk(name, it.Items)
			} else if it.Request != nil {
				out = append(out, namedItem{Name: name, Item: it})
			}
		}
	}
	walk("", c.Items)
	return out
}

// buildSpec 把请求转成归一化 RequestSpec，并完成变量替换。
func buildSpec(item Item, vars *VarStore) (*RequestSpec, error) {
	req := item.Request
	if req == nil {
		return nil, fmt.Errorf("不是请求项")
	}
	spec := &RequestSpec{
		Method:  strings.ToUpper(req.Method),
		Headers: map[string]string{},
	}
	if req.URL != nil {
		spec.URL = req.URL.String()
	}
	for _, h := range req.Header {
		if !h.EnabledOr() {
			continue
		}
		spec.Headers[h.Key] = h.Value
	}
	if req.Body != nil {
		switch req.Body.Mode {
		case "raw", "json", "graphql", "text", "xml":
			spec.Body = []byte(req.Body.Raw)
		case "urlencoded":
			spec.Body = []byte(encodeForm(req.Body.URLEncoded))
			if _, ok := spec.Headers["Content-Type"]; !ok {
				spec.Headers["Content-Type"] = "application/x-www-form-urlencoded"
			}
		case "formdata":
			spec.Body = []byte(encodeForm(req.Body.FormData))
		}
	}
	if spec.Method == "" {
		spec.Method = "GET"
	}
	vars.Expand(spec)
	return spec, nil
}

func encodeForm(kvs []KeyValue) string {
	form := url.Values{}
	for _, kv := range kvs {
		if !kv.EnabledOr() {
			continue
		}
		form.Set(kv.Key, kv.Value)
	}
	return form.Encode()
}

// doRequest 发送 HTTP 请求并返回归一化响应。
func (e *Engine) doRequest(ctx context.Context, spec *RequestSpec) (*Response, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, spec.Method, spec.URL, bytes.NewReader(spec.Body))
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	if len(spec.Body) > 0 {
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(spec.Body)))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 最多读 1MB
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, vs := range resp.Header {
		headers[k] = strings.Join(vs, ", ")
	}
	return &Response{
		Status:       resp.StatusCode,
		StatusText:   resp.Status,
		ResponseTime: time.Since(start).Milliseconds(),
		Headers:      headers,
		Body:         body,
	}, nil
}

// RunItem 运行单个请求并执行 test script，返回结果。
func (e *Engine) RunItem(ctx context.Context, ni namedItem, vars *VarStore) RequestResult {
	res := RequestResult{
		Name:   ni.Name,
		Method: strings.ToUpper(ni.Item.Request.Method),
	}
	spec, err := buildSpec(ni.Item, vars)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Method = spec.Method
	res.URL = spec.URL

	resp, err := e.doRequest(ctx, spec)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Status = resp.Status
	res.StatusText = resp.StatusText
	res.ResponseTime = resp.ResponseTime
	if len(resp.Body) > 500 {
		res.BodyPreview = string(resp.Body[:500]) + "..."
	} else {
		res.BodyPreview = string(resp.Body)
	}

	// 执行 test script
	pm := newPMRuntime(vars, resp)
	for _, ev := range ni.Item.Event {
		if ev.Listen == "test" {
			res.Tests = pm.RunScript(ev.Script.Exec)
		}
	}
	// 无显式断言时，默认以 2xx/3xx 为通过
	if len(res.Tests) == 0 {
		res.Passed = resp.Status >= 200 && resp.Status < 400
	} else {
		res.Passed = true
		for _, t := range res.Tests {
			if !t.Passed {
				res.Passed = false
				break
			}
		}
	}
	return res
}

// RunCollection 运行整个 collection 的所有请求。
func (e *Engine) RunCollection(ctx context.Context, coll *Collection, env *Environment) CollectionResult {
	items := coll.Flatten()
	vars := NewVarStore(coll, env)
	out := CollectionResult{
		Collection: coll.Info.Name,
		Total:      len(items),
		Requests:   make([]RequestResult, 0, len(items)),
	}
	for _, ni := range items {
		select {
		case <-ctx.Done():
			out.Requests = append(out.Requests, RequestResult{Name: ni.Name, Error: "已取消"})
			continue
		default:
		}
		r := e.RunItem(ctx, ni, vars)
		if r.Passed && r.Error == "" {
			out.Passed++
		} else {
			out.Failed++
		}
		out.Requests = append(out.Requests, r)
	}
	return out
}
