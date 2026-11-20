package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ServeStdio 以 stdio 传输运行 MCP Server：
// stdin 每行一条 JSON-RPC 消息，stdout 每行一条响应，日志一律走 stderr。
// 供 Kiro / Claude Desktop / Cursor / WorkBuddy 等以子进程方式拉起。
func (s *Server) ServeStdio() error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 32<<20) // 单行最大 32MB

	out := bufio.NewWriter(os.Stdout)
	var writeMu sync.Mutex

	writeResp := func(resp *Response) {
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = out.Write(data)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}

	var wg sync.WaitGroup
	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writeResp(errResp(nil, CodeParseError, "parse error: "+err.Error()))
			continue
		}
		// 并发处理：SSH/DB 慢调用不阻塞后续 ping 与工具列表请求
		wg.Add(1)
		go func(r Request) {
			defer wg.Done()
			resp, _ := s.dispatch(r)
			if resp != nil {
				writeResp(resp)
			}
		}(req)
	}
	wg.Wait()
	if err := in.Err(); err != nil {
		return fmt.Errorf("stdio read: %w", err)
	}
	return nil
}
