package builtins

import (
	"fmt"
	"strings"
	"sync"

	"mcp-ops/internal/mcp"
)

// RegisterSequentialThinking 注册官方 server-sequential-thinking 的 sequentialthinking 工具。
// 维护一个会话级思考历史，帮助模型分步、可修正地推理复杂问题。
func RegisterSequentialThinking(s *mcp.Server) {
	st := &seqThinker{}

	s.RegisterTool("sequentialthinking",
		"结构化逐步思考工具：用于拆解复杂问题、记录每一步推理、必要时修正或分叉之前的思考。"+
			"在需要多步分析（规划、调试、权衡）时调用，每步返回思考历史概览。",
		schema(`"thought":{"type":"string","description":"当前这一步的完整思考内容"},
			"nextThoughtNeeded":{"type":"boolean","description":"是否还需要继续思考"},
			"thoughtNumber":{"type":"integer","description":"当前是第几步（1 基）"},
			"totalThoughts":{"type":"integer","description":"预计总步数（可调整）"},
			"isRevision":{"type":"boolean","description":"这一步是否修正之前的某一步"},
			"revisesThought":{"type":"integer","description":"被修正的步骤编号"},
			"branchFromThought":{"type":"integer","description":"从哪一步分叉"},
			"branchId":{"type":"string","description":"分叉标识"},
			"needsMoreThoughts":{"type":"boolean"}`,
		"thought", "nextThoughtNeeded", "thoughtNumber", "totalThoughts"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult(st.record(seqInput{
				Thought:            argString(args, "thought"),
				NextThoughtNeeded:  argBool(args, "nextThoughtNeeded"),
				ThoughtNumber:      argInt(args, "thoughtNumber"),
				TotalThoughts:      argInt(args, "totalThoughts"),
				IsRevision:         argBool(args, "isRevision"),
				RevisesThought:     argInt(args, "revisesThought"),
				BranchFromThought:  argInt(args, "branchFromThought"),
				BranchID:           argString(args, "branchId"),
				NeedsMoreThoughts:  argBool(args, "needsMoreThoughts"),
			}))
		})
}

type seqInput struct {
	Thought            string
	NextThoughtNeeded  bool
	ThoughtNumber      int
	TotalThoughts      int
	IsRevision         bool
	RevisesThought     int
	BranchFromThought  int
	BranchID           string
	NeedsMoreThoughts  bool
}

type seqThinker struct {
	mu      sync.Mutex
	history []string
}

func (t *seqThinker) record(in seqInput) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	thought := strings.TrimSpace(in.Thought)
	if thought == "" {
		return "错误：thought 不能为空。请给出当前这一步的具体思考内容。"
	}
	num := in.ThoughtNumber
	if num <= 0 {
		num = len(t.history) + 1
	}

	label := fmt.Sprintf("步骤 %d", num)
	switch {
	case in.IsRevision && in.RevisesThought > 0:
		label = fmt.Sprintf("步骤 %d（修正步骤 %d）", num, in.RevisesThought)
	case in.BranchFromThought > 0:
		label = fmt.Sprintf("步骤 %d（从步骤 %d 分叉）", num, in.BranchFromThought)
	}

	entry := fmt.Sprintf("%s: %s", label, thought)
	// 记录在对应位置：简单起见追加到历史尾部（保持顺序）
	if num > len(t.history) {
		t.history = append(t.history, entry)
	} else if num > 0 {
		t.history[num-1] = entry
	} else {
		t.history = append(t.history, entry)
	}

	var b strings.Builder
	b.WriteString("已记录思考。当前思考历史：\n\n")
	for _, h := range t.history {
		b.WriteString("- " + h + "\n")
	}

	total := in.TotalThoughts
	if total > 0 {
		fmt.Fprintf(&b, "\n进度：%d / %d 步\n", num, total)
	}
	switch {
	case in.NeedsMoreThoughts || in.NextThoughtNeeded:
		b.WriteString("\n建议：继续下一步思考。请给出 thought、递增的 thoughtNumber，并在无需继续时设 nextThoughtNeeded=false。")
	case !in.NextThoughtNeeded:
		b.WriteString("\n已完成思考，可基于上述历史给出最终结论。")
	}
	return b.String()
}
