package dbsrv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mcp-ops/internal/config"
)

// esDriver 轻量 Elasticsearch REST 客户端（不引重型官方 SDK）。
//
// Query 与 Execute 的参数统一为 JSON：
//	{"method":"GET","path":"/my-index/_search","body":{"query":{"match_all":{}},"size":10}}
//	Query 限定 GET/POST/HEAD（读语义）；Execute 放开 PUT/POST/DELETE（写语义）。
//	特殊路径 {"method":"GET","path":"/_cat/indices?v"} 之类原样直通。
type esDriver struct {
	baseURL string
	auth    string
	client  *http.Client
}

func openES(cfg *config.Database, addr string) (*esDriver, error) {
	scheme := "http"
	if strings.HasPrefix(cfg.Host, "https://") {
		scheme = ""
	}
	host := cfg.Host
	if scheme != "" {
		host = scheme + "://" + addr
	} else if cfg.ViaSSH != "" {
		host = "http://" + addr
	}
	d := &esDriver{
		baseURL: strings.TrimRight(host, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	if cfg.User != "" {
		d.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.User+":"+cfg.Password))
	}
	// 探活
	req, _ := http.NewRequest(http.MethodGet, d.baseURL+"/", nil)
	if _, err := d.do(context.Background(), req); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *esDriver) Type() string { return "elasticsearch" }
func (d *esDriver) Close()       {}

type esOp struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

func (d *esDriver) run(ctx context.Context, q string, allowWrite bool) (string, error) {
	var op esOp
	if err := json.Unmarshal([]byte(q), &op); err != nil {
		return "", fmt.Errorf("argument must be {\"method\",\"path\",\"body\"} JSON: %w", err)
	}
	if op.Path == "" || !strings.HasPrefix(op.Path, "/") {
		return "", fmt.Errorf("path must start with /")
	}
	method := strings.ToUpper(op.Method)
	if method == "" {
		method = http.MethodGet
	}
	readOK := method == http.MethodGet || method == http.MethodHead || method == http.MethodPost
	switch {
	case !allowWrite && !readOK:
		return "", fmt.Errorf("db_query only allows GET/HEAD/POST; %s is write semantics, use db_execute", method)
	case allowWrite && method != http.MethodGet && method != http.MethodHead && method != http.MethodPost &&
		method != http.MethodPut && method != http.MethodDelete:
		return "", fmt.Errorf("unsupported method %q", method)
	}

	req, err := http.NewRequestWithContext(ctx, method, d.baseURL+op.Path, nil)
	if err != nil {
		return "", err
	}
	if len(op.Body) > 0 {
		req.Body = io.NopCloser(bytes.NewReader(op.Body))
		req.Header.Set("Content-Type", "application/json")
	}
	return d.do(ctx, req)
}

func (d *esDriver) Query(ctx context.Context, q string, _ []string) (string, error) {
	return d.run(ctx, q, false)
}

func (d *esDriver) Execute(ctx context.Context, q string, _ []string) (string, error) {
	return d.run(ctx, q, true)
}

func (d *esDriver) do(ctx context.Context, req *http.Request) (string, error) {
	if d.auth != "" {
		req.Header.Set("Authorization", d.auth)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("elasticsearch http %d: %s", resp.StatusCode, truncateStr(string(body), 2048))
	}
	return string(body), nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
