package postmansrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-ops/internal/config"
)

func TestVarReplace(t *testing.T) {
	coll := &Collection{Variables: []Variable{{Key: "base", Value: "http://example.com"}}}
	env := &Environment{Values: []Variable{{Key: "token", Value: "abc123"}}}
	v := NewVarStore(coll, env)

	cases := map[string]string{
		"{{base}}/api":            "http://example.com/api",
		"Bearer {{token}}":        "Bearer abc123",
		"{{missing}}":             "{{missing}}", // 未定义保留原文
		"{{base}}?t={{token}}":    "http://example.com?t=abc123",
	}
	for in, want := range cases {
		if got := v.Replace(in); got != want {
			t.Errorf("Replace(%q) = %q, want %q", in, got, want)
		}
	}
	// 动态变量
	if got := v.Replace("{{$timestamp}}"); got == "{{$timestamp}}" {
		t.Error("$timestamp 应被替换")
	}
	if got := v.Replace("{{$guid}}"); strings.Contains(got, "{{") {
		t.Errorf("$guid 应被替换，得到 %q", got)
	}
}

func TestFlatten(t *testing.T) {
	c := &Collection{}
	c.Info.Name = "demo"
	c.Items = []Item{
		{Name: "root1", Request: &Request{Method: "GET", URL: &URL{Raw: "http://x/1"}}},
		{Name: "folder", Items: []Item{
			{Name: "child1", Request: &Request{Method: "POST", URL: &URL{Raw: "http://x/2"}}},
		}},
	}
	flat := c.Flatten()
	if len(flat) != 2 {
		t.Fatalf("期望 2 个请求，得到 %d", len(flat))
	}
	if flat[0].Name != "root1" {
		t.Errorf("flat[0] = %q", flat[0].Name)
	}
	if flat[1].Name != "folder / child1" {
		t.Errorf("flat[1] = %q", flat[1].Name)
	}
}

func TestBuildSpec(t *testing.T) {
	coll := &Collection{Variables: []Variable{{Key: "host", Value: "http://h"}}}
	v := NewVarStore(coll, nil)
	item := Item{
		Name: "req",
		Request: &Request{
			Method: "post",
			Header: []KeyValue{{Key: "X-Token", Value: "{{host}}"}},
			URL:    &URL{Raw: "{{host}}/api"},
			Body:   &Body{Mode: "raw", Raw: `{"a":"{{host}}"}`},
		},
	}
	spec, err := buildSpec(item, v)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Method != "POST" {
		t.Errorf("method = %q", spec.Method)
	}
	if spec.URL != "http://h/api" {
		t.Errorf("url = %q", spec.URL)
	}
	if spec.Headers["X-Token"] != "http://h" {
		t.Errorf("header = %q", spec.Headers["X-Token"])
	}
	if string(spec.Body) != `{"a":"http://h"}` {
		t.Errorf("body = %q", spec.Body)
	}
}

func TestPMAssertionsPass(t *testing.T) {
	resp := &Response{
		Status:       200,
		StatusText:   "200 OK",
		ResponseTime: 120,
		Headers:      map[string]string{"Content-Type": "application/json"},
		Body:         []byte(`{"id":1,"name":"test"}`),
	}
	pm := newPMRuntime(NewVarStore(nil, nil), resp)
	results := pm.RunScript([]string{
		`pm.test("status", function(){ pm.response.to.have.status(200); });`,
		`pm.test("json id", function(){ pm.expect(pm.response.json().id).to.equal(1); });`,
		`pm.test("include", function(){ pm.expect(pm.response.text()).to.include("test"); });`,
		`pm.test("below", function(){ pm.expect(pm.response.responseTime).to.be.below(500); });`,
		`pm.test("not equal", function(){ pm.expect(pm.response.code).to.not.equal(500); });`,
		`pm.test("property", function(){ pm.expect(pm.response.json()).to.have.property("name"); });`,
	})
	for _, r := range results {
		if !r.Passed {
			t.Fatalf("断言 %q 意外失败: %s", r.Name, r.Error)
		}
	}
	if len(results) != 6 {
		t.Fatalf("期望 6 个断言，得到 %d", len(results))
	}
}

func TestPMAssertionFail(t *testing.T) {
	resp := &Response{Status: 404, Body: []byte("not found")}
	pm := newPMRuntime(NewVarStore(nil, nil), resp)
	results := pm.RunScript([]string{
		`pm.test("should fail", function(){ pm.expect(1).to.equal(2); });`,
		`pm.test("should pass", function(){ pm.expect(1).to.equal(1); });`,
	})
	if len(results) != 2 {
		t.Fatalf("期望 2 个断言，得到 %d", len(results))
	}
	if results[0].Passed {
		t.Error("第一个断言应失败")
	}
	if results[0].Error == "" {
		t.Error("失败断言应有错误信息")
	}
	if !results[1].Passed {
		t.Error("第二个断言应通过")
	}
}

func TestRunCollectionEndToEnd(t *testing.T) {
	// 本地测试服务器
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer srv.Close()

	// 写 collection 文件
	collPath := filepath.Join(t.TempDir(), "c.json")
	collJSON := `{
		"info": {"name": "demo", "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},
		"variable": [{"key": "base", "value": "` + srv.URL + `"}],
		"item": [
			{"name": "health", "request": {"method": "GET", "url": {"raw": "{{base}}/health"}},
			 "event": [{"listen": "test", "script": {"exec": [
				"pm.test('status 200', function(){ pm.response.to.have.status(200); });",
				"pm.test('json ok', function(){ pm.expect(pm.response.json().ok).to.equal(true); });",
				"pm.test('path', function(){ pm.expect(pm.response.json().path).to.include('health'); });"
			 ]}}]}
		]
	}`
	if err := os.WriteFile(collPath, []byte(collJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	coll, err := LoadCollection(collPath)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(&config.PostmanConfig{})
	res := eng.RunCollection(context.Background(), coll, nil)

	if res.Total != 1 {
		t.Fatalf("期望 1 个请求，得到 %d", res.Total)
	}
	if res.Passed != 1 || res.Failed != 0 {
		t.Fatalf("期望全部通过，passed=%d failed=%d", res.Passed, res.Failed)
	}
	r := res.Requests[0]
	if r.Status != 200 {
		t.Errorf("status = %d", r.Status)
	}
	if len(r.Tests) != 3 {
		t.Fatalf("期望 3 个断言，得到 %d", len(r.Tests))
	}
	for _, tt := range r.Tests {
		if !tt.Passed {
			t.Errorf("断言 %q 失败: %s", tt.Name, tt.Error)
		}
	}
}
