package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// InProcServer 把进程内的 Server 包装成 ToolServer，
// 让内置能力（SSH / DB / Web）与外部 stdio server 走同一套注册与调用路径。
type InProcServer struct {
	name string
	srv  *Server
}

func NewInProcServer(name string, srv *Server) *InProcServer {
	return &InProcServer{name: name, srv: srv}
}

func (i *InProcServer) Name() string      { return i.name }
func (i *InProcServer) Initialize() error { return nil }
func (i *InProcServer) Close() error      { return nil }

func (i *InProcServer) ListTools() ([]Tool, error) {
	req := Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage(`1`), Method: MethodToolsList}
	resp, _ := i.srv.dispatch(context.Background(), req)
	if resp == nil || resp.Error != nil {
		return nil, fmt.Errorf("tools/list: %v", respError(resp))
	}
	var out ListToolsResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

func (i *InProcServer) CallTool(name string, args map[string]any) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := ToolsCallParams{Name: name, Arguments: args}
	raw, _ := json.Marshal(params)
	req := Request{JSONRPC: JSONRPCVersion, ID: json.RawMessage(`1`), Method: MethodToolsCall, Params: raw}
	resp, _ := i.srv.dispatch(context.Background(), req)
	if resp == nil || resp.Error != nil {
		return nil, fmt.Errorf("tools/call %s: %v", name, respError(resp))
	}
	var out CallToolResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func respError(resp *Response) any {
	if resp == nil {
		return "no response"
	}
	if resp.Error != nil {
		return fmt.Sprintf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return "empty result"
}
