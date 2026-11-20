package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func dispatchJSON(t *testing.T, s *Server, method string, params any) *Response {
	t.Helper()
	raw, _ := json.Marshal(params)
	req := Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage(`1`), Method: method, Params: raw}
	resp, _ := s.dispatch(context.Background(), req)
	return resp
}

func TestServerResourcesAndPrompts(t *testing.T) {
	s := NewServer(Implementation{Name: "test", Version: "1"}, "")

	// 静态资源
	s.RegisterResource(Resource{URI: "config://app", Name: "app 配置", MimeType: "text/plain", Text: "a=b\nc=d"})
	// 动态资源
	s.RegisterResourceHandler("runtime://status", func(ctx CallContext, uri string) (*ResourceContents, error) {
		return &ResourceContents{URI: uri, MimeType: "application/json", Text: `{"ok":true}`}, nil
	})
	// 资源模板
	s.RegisterResourceTemplate(ResourceTemplate{URITemplate: "docs://{name}", Name: "文档", MimeType: "text/markdown"})
	// 提示词（动态）
	s.RegisterPromptHandler("summarize", func(ctx CallContext, args map[string]any) ([]PromptMessage, error) {
		topic, _ := args["topic"].(string)
		return []PromptMessage{{Role: "user", Content: Content{Type: "text", Text: "请总结：" + topic}}}, nil
	})

	// resources/list
	resp := dispatchJSON(t, s, MethodResourcesList, map[string]any{})
	var list ListResourcesResult
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Resources) != 1 || list.Resources[0].URI != "config://app" {
		t.Errorf("resources/list 应返回 1 个静态资源，得到 %+v", list.Resources)
	}

	// resources/read 静态
	resp = dispatchJSON(t, s, MethodResourcesRead, ReadResourceParams{URI: "config://app"})
	var rd ReadResourceResult
	if err := json.Unmarshal(resp.Result, &rd); err != nil {
		t.Fatal(err)
	}
	if len(rd.Contents) != 1 || !strings.Contains(rd.Contents[0].Text, "a=b") {
		t.Errorf("resources/read 静态资源失败：%+v", rd.Contents)
	}

	// resources/read 动态
	resp = dispatchJSON(t, s, MethodResourcesRead, ReadResourceParams{URI: "runtime://status"})
	if err := json.Unmarshal(resp.Result, &rd); err != nil {
		t.Fatal(err)
	}
	if len(rd.Contents) != 1 || !strings.Contains(rd.Contents[0].Text, "ok") {
		t.Errorf("resources/read 动态资源失败：%+v", rd.Contents)
	}

	// 未知资源
	resp = dispatchJSON(t, s, MethodResourcesRead, ReadResourceParams{URI: "nope://x"})
	if resp.Error == nil {
		t.Error("未知资源应返回错误")
	}

	// resources/templates/list
	resp = dispatchJSON(t, s, MethodResourcesTemplatesList, map[string]any{})
	var tpls ListResourceTemplatesResult
	if err := json.Unmarshal(resp.Result, &tpls); err != nil {
		t.Fatal(err)
	}
	if len(tpls.ResourceTemplates) != 1 || tpls.ResourceTemplates[0].URITemplate != "docs://{name}" {
		t.Errorf("templates/list 失败：%+v", tpls.ResourceTemplates)
	}

	// prompts/list
	resp = dispatchJSON(t, s, MethodPromptsList, map[string]any{})
	var pl ListPromptsResult
	if err := json.Unmarshal(resp.Result, &pl); err != nil {
		t.Fatal(err)
	}
	if len(pl.Prompts) != 1 || pl.Prompts[0].Name != "summarize" {
		t.Errorf("prompts/list 失败：%+v", pl.Prompts)
	}

	// prompts/get
	resp = dispatchJSON(t, s, MethodPromptsGet, GetPromptParams{Name: "summarize", Arguments: map[string]any{"topic": "MCP"}})
	var gp GetPromptResult
	if err := json.Unmarshal(resp.Result, &gp); err != nil {
		t.Fatal(err)
	}
	if len(gp.Messages) != 1 || !strings.Contains(gp.Messages[0].Content.Text, "MCP") {
		t.Errorf("prompts/get 失败：%+v", gp.Messages)
	}
}

func TestContentMultiTypeResultText(t *testing.T) {
	r := &CallToolResult{Content: []Content{
		{Type: "text", Text: "hello"},
		{Type: "image", Data: "AAAA", MimeType: "image/png"},
		{Type: "resource", URI: "docs://x", MimeType: "text/markdown"},
		{Type: "embedded_resource", Resource: &EmbeddedResource{URI: "file:///a.txt", MimeType: "text/plain", Text: "inline"}},
	}}
	got := r.ResultText()
	for _, want := range []string{"hello", "[image: image/png", "[resource: docs://x", "[embedded_resource: file:///a.txt", "inline"} {
		if !strings.Contains(got, want) {
			t.Errorf("ResultText 应包含 %q，得到：%q", want, got)
		}
	}
}

func TestProgressNotification(t *testing.T) {
	s := NewServer(Implementation{Name: "test", Version: "1"}, "")

	var progressCalls []ProgressParams
	s.SetNotifier(func(method string, params any) {
		if method == MethodNotificationsProgress {
			if p, ok := params.(ProgressParams); ok {
				progressCalls = append(progressCalls, p)
			}
		}
	})

	s.RegisterTool("slow", "慢工具", json.RawMessage(`{"type":"object"}`),
		func(ctx CallContext, args map[string]any) *CallToolResult {
			if ctx.Progress != nil {
				ctx.Progress(0.5, 1.0, "half done")
				ctx.Progress(1.0, 1.0, "done")
			}
			return TextResult("ok")
		})

	raw, _ := json.Marshal(ToolsCallParams{Name: "slow", ProgressToken: "tok-1"})
	req := Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage(`1`), Method: MethodToolsCall, Params: raw}
	s.dispatch(context.Background(), req)

	if len(progressCalls) != 2 {
		t.Fatalf("期望 2 次 progress 通知，得到 %d", len(progressCalls))
	}
	if progressCalls[0].Progress != 0.5 || progressCalls[0].Message != "half done" {
		t.Errorf("第一次进度不符：%+v", progressCalls[0])
	}
	if progressCalls[0].ProgressToken != "tok-1" {
		t.Errorf("progressToken 应透传，得到 %v", progressCalls[0].ProgressToken)
	}
}

func TestProgressTokenMetaExtraction(t *testing.T) {
	// 新版 spec 把 progressToken 放在 _meta 里
	raw := []byte(`{"name":"x","_meta":{"progressToken":"abc"}}`)
	var p ToolsCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.token() != "abc" {
		t.Errorf("应从 _meta 提取 progressToken，得到 %v", p.token())
	}
	// 旧版顶层
	raw2 := []byte(`{"name":"x","progressToken":123}`)
	if err := json.Unmarshal(raw2, &p); err != nil {
		t.Fatal(err)
	}
	if p.token() != float64(123) {
		t.Errorf("应从顶层提取 progressToken，得到 %v", p.token())
	}
}
