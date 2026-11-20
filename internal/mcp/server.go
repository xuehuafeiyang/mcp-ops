package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server 是通用 MCP 服务端核心：工具注册 + 传输无关的 RPC 分发。
// 传输层有两种实现：
//   - Streamable HTTP（Handler）：常驻服务，Bearer token + session
//   - stdio（ServeStdio）：子进程模式，供 Kiro/Claude Desktop/Cursor 等客户端拉起
type Server struct {
	info     Implementation
	token    string
	mu       sync.RWMutex
	tools    map[string]Tool
	handlers map[string]ToolHandler

	sessionMu   sync.Mutex
	sessions    map[string]bool
	nextSession int
}

func NewServer(info Implementation, token string) *Server {
	return &Server{
		info:     info,
		token:    token,
		tools:    map[string]Tool{},
		handlers: map[string]ToolHandler{},
		sessions: map[string]bool{},
	}
}

// RegisterTool 注册工具；inputSchema 为 JSON Schema 的 json.RawMessage
func (s *Server) RegisterTool(name, description string, inputSchema json.RawMessage, h ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = Tool{Name: name, Description: description, InputSchema: inputSchema}
	s.handlers[name] = h
}

// ---------- 传输无关的分发 ----------

// dispatch 处理一条 JSON-RPC 请求并返回响应。
// 返回 nil 表示这是 notification，无需响应。
// 返回的 sessionID 由 initialize 生成（stdio 传输可忽略）。
func (s *Server) dispatch(req Request) (resp *Response, sessionID string) {
	if req.JSONRPC != JSONRPCVersion {
		return errResp(req.ID, CodeInvalidRequest, "jsonrpc must be \"2.0\""), ""
	}
	if req.IsNotification() {
		return nil, ""
	}

	cancel := make(chan struct{})
	defer close(cancel)
	ctx := CallContext{Cancel: cancel}

	switch req.Method {
	case MethodInitialize:
		res := InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    ServerCaps{},
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
		var params ToolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, CodeInvalidParams, "tools/call params: "+err.Error()), ""
		}
		s.mu.RLock()
		h, ok := s.handlers[params.Name]
		s.mu.RUnlock()
		if !ok {
			return errResp(req.ID, CodeInvalidParams, "unknown tool: "+params.Name), ""
		}
		result := h(ctx, params.Arguments)
		if result == nil {
			result = ErrResult("tool returned nil result")
		}
		res, _ := json.Marshal(result)
		return okResp(req.ID, res), ""
	default:
		return errResp(req.ID, CodeMethodNotFound, "method not found: "+req.Method), ""
	}
}

func okResp(id json.RawMessage, result json.RawMessage) *Response {
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string) *Response {
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Error: &RPCError{Code: code, Message: msg}}
}

// ---------- Streamable HTTP 传输 ----------

// Handler 返回 http.Handler，path 参数为挂载路径（例如 "/mcp"）
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

	resp, sid := s.dispatch(req)
	if sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	status := http.StatusOK
	if resp != nil && resp.Error != nil && (resp.Error.Code == CodeParseError || resp.Error.Code == CodeInvalidRequest) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
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
