package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server 是通用 MCP 服务端核心：工具/资源/提示词注册 + 传输无关的 RPC 分发。
// 传输层有两种实现：
//   - Streamable HTTP（Handler）：常驻服务，Bearer token + session + SSE 进度流
//   - stdio（ServeStdio）：子进程模式，供 Kiro/Claude Desktop/Cursor 等客户端拉起
type Server struct {
	info     Implementation
	token    string
	mu       sync.RWMutex
	tools    map[string]Tool
	handlers map[string]ToolHandler

	resources         map[string]Resource
	resourceHandlers  map[string]ResourceHandler
	resourceTemplates []ResourceTemplate
	prompts           map[string]Prompt
	promptHandlers    map[string]PromptHandler

	// notifier 由传输层注入：把 notifications/progress 等通知发给客户端。
	notifier func(method string, params any)

	// activeCalls 记录进行中的工具调用，用于 notifications/cancelled 取消。
	activeMu    sync.Mutex
	activeCalls map[string]context.CancelFunc

	sessionMu   sync.Mutex
	sessions    map[string]bool
	nextSession int
}

func NewServer(info Implementation, token string) *Server {
	return &Server{
		info:             info,
		token:            token,
		tools:            map[string]Tool{},
		handlers:         map[string]ToolHandler{},
		resources:        map[string]Resource{},
		resourceHandlers: map[string]ResourceHandler{},
		prompts:          map[string]Prompt{},
		promptHandlers:   map[string]PromptHandler{},
		sessions:         map[string]bool{},
		activeCalls:      map[string]context.CancelFunc{},
	}
}

// RegisterTool 注册工具；inputSchema 为 JSON Schema 的 json.RawMessage。
func (s *Server) RegisterTool(name, description string, inputSchema json.RawMessage, h ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = Tool{Name: name, Description: description, InputSchema: inputSchema}
	s.handlers[name] = h
}

// RegisterResource 注册静态资源（内容在注册时固定）。
func (s *Server) RegisterResource(r Resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[r.URI] = r
}

// RegisterResourceHandler 注册动态资源读取器（按 URI 精确匹配）。
func (s *Server) RegisterResourceHandler(uri string, h ResourceHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceHandlers[uri] = h
}

// RegisterResourceTemplate 声明一个资源模板（用于 resources/templates/list）。
func (s *Server) RegisterResourceTemplate(t ResourceTemplate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceTemplates = append(s.resourceTemplates, t)
}

// RegisterPrompt 注册静态提示词。
func (s *Server) RegisterPrompt(p Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts[p.Name] = p
}

// RegisterPromptHandler 注册动态提示词生成器。
func (s *Server) RegisterPromptHandler(name string, h PromptHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 也登记一个 Prompt 元信息，方便 prompts/list 展示
	if _, ok := s.prompts[name]; !ok {
		s.prompts[name] = Prompt{Name: name}
	}
	s.promptHandlers[name] = h
}

// SetNotifier 由传输层注入通知发送器（progress 等）。
func (s *Server) SetNotifier(n func(method string, params any)) {
	s.notifier = n
}

// ---------- 传输无关的分发 ----------

// dispatch 处理一条 JSON-RPC 请求并返回响应。
// parent 用于感知传输层断开（HTTP 客户端断开 / cancelled 通知）。
// 返回 nil 表示这是 notification，无需响应。
// 返回的 sessionID 由 initialize 生成（stdio 传输可忽略）。
func (s *Server) dispatch(parent context.Context, req Request) (resp *Response, sessionID string) {
	if req.JSONRPC != JSONRPCVersion {
		return errResp(req.ID, CodeInvalidRequest, "jsonrpc must be \"2.0\""), ""
	}
	if req.IsNotification() {
		s.handleNotification(req)
		return nil, ""
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	callCtx := CallContext{Cancel: ctx.Done()}

	switch req.Method {
	case MethodInitialize:
		caps := ServerCaps{
			Tools: &struct {
				ListChanged bool `json:"listChanged,omitempty"`
			}{ListChanged: true},
			Resources: &struct {
				Subscribe   bool `json:"subscribe,omitempty"`
				ListChanged bool `json:"listChanged,omitempty"`
			}{Subscribe: true, ListChanged: true},
			Prompts: &struct {
				ListChanged bool `json:"listChanged,omitempty"`
			}{ListChanged: true},
			Logging: &struct{}{},
		}
		res := InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    caps,
			ServerInfo:      s.info,
		}
		result, _ := json.Marshal(res)
		return okResp(req.ID, result), s.newSession()
	case MethodPing:
		return okResp(req.ID, json.RawMessage(`{}`)), ""
	case MethodToolsList:
		s.mu.RLock()
		tools := make([]Tool, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, t)
		}
		s.mu.RUnlock()
		res, _ := json.Marshal(ListToolsResult{Tools: tools})
		return okResp(req.ID, res), ""
	case MethodToolsCall:
		s.registerActiveCall(req.ID, cancel)
		defer s.unregisterActiveCall(req.ID)
		return s.dispatchToolCall(req, callCtx), ""
	case MethodResourcesList:
		s.mu.RLock()
		resources := make([]Resource, 0, len(s.resources))
		for _, r := range s.resources {
			resources = append(resources, r)
		}
		s.mu.RUnlock()
		res, _ := json.Marshal(ListResourcesResult{Resources: resources})
		return okResp(req.ID, res), ""
	case MethodResourcesRead:
		var params ReadResourceParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, CodeInvalidParams, "resources/read params: "+err.Error()), ""
		}
		return s.dispatchResourceRead(req, callCtx, params.URI), ""
	case MethodResourcesTemplatesList:
		s.mu.RLock()
		tpls := append([]ResourceTemplate{}, s.resourceTemplates...)
		s.mu.RUnlock()
		res, _ := json.Marshal(ListResourceTemplatesResult{ResourceTemplates: tpls})
		return okResp(req.ID, res), ""
	case MethodPromptsList:
		s.mu.RLock()
		prompts := make([]Prompt, 0, len(s.prompts))
		for _, p := range s.prompts {
			prompts = append(prompts, p)
		}
		s.mu.RUnlock()
		res, _ := json.Marshal(ListPromptsResult{Prompts: prompts})
		return okResp(req.ID, res), ""
	case MethodPromptsGet:
		var params GetPromptParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, CodeInvalidParams, "prompts/get params: "+err.Error()), ""
		}
		return s.dispatchPromptGet(req, callCtx, params), ""
	case MethodRootsList:
		// roots 由客户端向服务器声明工作目录；本服务器为工具型，返回空清单。
		res, _ := json.Marshal(RootsListResult{Roots: []Root{}})
		return okResp(req.ID, res), ""
	case MethodLoggingSetLevel:
		// 接受并忽略：服务端日志级别由自身进程控制，不做动态调整。
		return okResp(req.ID, json.RawMessage(`{}`)), ""
	default:
		return errResp(req.ID, CodeMethodNotFound, "method not found: "+req.Method), ""
	}
}

func (s *Server) dispatchToolCall(req Request, ctx CallContext) *Response {
	var params ToolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errResp(req.ID, CodeInvalidParams, "tools/call params: "+err.Error())
	}
	s.mu.RLock()
	h, ok := s.handlers[params.Name]
	s.mu.RUnlock()
	if !ok {
		return errResp(req.ID, CodeInvalidParams, "unknown tool: "+params.Name)
	}

	// 进度回传：工具内调用 ctx.Progress 时，通过注入的 notifier 发 notification。
	if s.notifier != nil {
		tok := params.token()
		ctx.Progress = func(progress, total float64, message string) {
			s.notifier(MethodNotificationsProgress, ProgressParams{
				ProgressToken: tok,
				Progress:      progress,
				Total:         total,
				Message:       message,
			})
		}
	}

	result := h(ctx, params.Arguments)
	if result == nil {
		result = ErrResult("tool returned nil result")
	}
	res, _ := json.Marshal(result)
	return okResp(req.ID, res)
}

func (s *Server) dispatchResourceRead(req Request, ctx CallContext, uri string) *Response {
	// 1) 静态资源
	s.mu.RLock()
	r, ok := s.resources[uri]
	s.mu.RUnlock()
	if ok {
		c := ResourceContents{URI: r.URI, MimeType: r.MimeType, Text: r.Text, Blob: r.Blob}
		res, _ := json.Marshal(ReadResourceResult{Contents: []ResourceContents{c}})
		return okResp(req.ID, res)
	}
	// 2) 动态 handler（精确匹配）
	s.mu.RLock()
	h, ok := s.resourceHandlers[uri]
	s.mu.RUnlock()
	if ok {
		c, err := h(ctx, uri)
		if err != nil {
			return errResp(req.ID, CodeInvalidParams, "resource read: "+err.Error())
		}
		res, _ := json.Marshal(ReadResourceResult{Contents: []ResourceContents{*c}})
		return okResp(req.ID, res)
	}
	return errResp(req.ID, CodeInvalidParams, "unknown resource: "+uri)
}

func (s *Server) dispatchPromptGet(req Request, ctx CallContext, params GetPromptParams) *Response {
	s.mu.RLock()
	p, staticOK := s.prompts[params.Name]
	h, dynOK := s.promptHandlers[params.Name]
	s.mu.RUnlock()

	var messages []PromptMessage
	if dynOK {
		var err error
		messages, err = h(ctx, params.Arguments)
		if err != nil {
			return errResp(req.ID, CodeInvalidParams, "prompt: "+err.Error())
		}
	} else if staticOK {
		// 静态 prompt：构造一条 user 消息（内容为占位文本；更丰富内容由 handler 提供）
		messages = []PromptMessage{{Role: "user", Content: Content{Type: "text", Text: p.Description}}}
	} else {
		return errResp(req.ID, CodeInvalidParams, "unknown prompt: "+params.Name)
	}
	res, _ := json.Marshal(GetPromptResult{Messages: messages})
	return okResp(req.ID, res)
}

// handleNotification 处理无需响应的通知消息。
func (s *Server) handleNotification(req Request) {
	switch req.Method {
	case MethodNotificationsCancelled:
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &params) == nil && len(params.RequestID) > 0 {
			s.cancelActiveCall(string(params.RequestID))
		}
	case MethodNotificationsMessage, MethodToolsListChanged, MethodResourcesListChanged, MethodPromptsListChanged:
		// 忽略
	}
}

func (s *Server) registerActiveCall(id json.RawMessage, cancel context.CancelFunc) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	s.activeCalls[string(id)] = cancel
}

func (s *Server) unregisterActiveCall(id json.RawMessage) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	delete(s.activeCalls, string(id))
}

func (s *Server) cancelActiveCall(id string) {
	s.activeMu.Lock()
	cancel, ok := s.activeCalls[id]
	delete(s.activeCalls, id)
	s.activeMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func okResp(id json.RawMessage, result json.RawMessage) *Response {
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string) *Response {
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Error: &RPCError{Code: code, Message: msg}}
}

// ---------- Streamable HTTP 传输 ----------

// Handler 返回 http.Handler，path 参数为挂载路径（例如 "/mcp"）。
func (s *Server) Handler(path string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.serveHTTP)
	return mux
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed: only POST is supported", http.StatusMethodNotAllowed)
		return
	}

	// Bearer token 认证（常量时间比较）
	auth := r.Header.Get("Authorization")
	got := strings.TrimPrefix(auth, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 8MB 上限
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp(nil, CodeParseError, "parse error: "+err.Error()))
		return
	}

	// notification：接受但不处理，返回 202（协议允许）
	if req.IsNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// initialize 之后要求携带 session id（若服务器已签发）
	if req.Method != MethodInitialize && !s.validSession(r.Header.Get("Mcp-Session-Id")) {
		writeJSON(w, http.StatusBadRequest, errResp(req.ID, CodeInvalidRequest,
			"missing or invalid Mcp-Session-Id; call initialize first"))
		return
	}

	// SSE 流式：仅当客户端声明接受 event-stream 且请求带进度令牌时启用，
	// 用于工具执行过程中回传 notifications/progress。
	if wantsSSE(r) && s.hasProgressToken(req) {
		s.serveSSE(w, r, req)
		return
	}

	// 普通模式：进度通知无法提前发出，静默丢弃
	old := s.notifier
	s.notifier = nil
	resp, sid := s.dispatch(r.Context(), req)
	s.notifier = old

	if sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	status := http.StatusOK
	if resp != nil && resp.Error != nil && (resp.Error.Code == CodeParseError || resp.Error.Code == CodeInvalidRequest) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
}

// serveSSE 以 text/event-stream 返回：先流式发 progress 事件，最后发 JSON-RPC 结果。
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, req Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// 环境不支持流式，退化为普通响应
		resp, _ := s.dispatch(r.Context(), req)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// 注入 notifier：progress → SSE 事件
	var mu sync.Mutex
	s.notifier = func(method string, params any) {
		if method != MethodNotificationsProgress {
			return
		}
		p, _ := json.Marshal(params)
		n := Notification{JSONRPC: JSONRPCVersion, Method: method, Params: p}
		b, _ := json.Marshal(n)
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		flusher.Flush()
	}

	resp, sid := s.dispatch(r.Context(), req)
	s.notifier = nil

	if sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	b, _ := json.Marshal(resp)
	mu.Lock()
	defer mu.Unlock()
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	flusher.Flush()
}

// Notification 是服务端主动推送的通知消息（progress 等）。
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func wantsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func (s *Server) hasProgressToken(req Request) bool {
	var params ToolsCallParams
	if req.Method != MethodToolsCall {
		return false
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return false
	}
	return params.token() != nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) newSession() string {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.nextSession++
	sid := fmt.Sprintf("sess-%d-%d", s.nextSession, time.Now().UnixNano())
	s.sessions[sid] = true
	return sid
}

func (s *Server) validSession(sid string) bool {
	if sid == "" {
		return false
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	return s.sessions[sid]
}
