package builtins

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-ops/internal/mcp"
)

// RegisterFilesystem 注册官方 server-filesystem 的工具集。
// allowedDirs 为允许访问的根目录列表（所有路径必须落在其中，含符号链接解析后）。
func RegisterFilesystem(s *mcp.Server, allowedDirs []string) {
	fs := &filesystem{allowed: allowedDirs}

	s.RegisterTool("read_file",
		"读取单个文本文件并返回其内容（带行号）。路径必须位于允许的目录内。",
		schema(`"path":{"type":"string","description":"文件路径"}`, "path"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			data, err := fs.readFile(argString(args, "path"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(data)
		})

	s.RegisterTool("read_multiple_files",
		"一次读取多个文件，返回每个文件的内容。",
		schema(`"paths":{"type":"array","items":{"type":"string"},"description":"文件路径列表"}`, "paths"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			paths := argStrings(args, "paths")
			if len(paths) == 0 {
				return errR("paths is required")
			}
			var b strings.Builder
			for _, p := range paths {
				data, err := fs.readFile(p)
				if err != nil {
					fmt.Fprintf(&b, "=== %s ===\nERROR: %v\n\n", p, err)
					continue
				}
				fmt.Fprintf(&b, "=== %s ===\n%s\n\n", p, data)
			}
			return mcp.TextResult(b.String())
		})

	s.RegisterTool("write_file",
		"创建或覆盖写入一个文件。父目录不存在时自动创建。",
		schema(`"path":{"type":"string","description":"文件路径"},"content":{"type":"string","description":"写入内容"}`, "path", "content"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			content := argString(args, "content")
			if p == "" {
				return errR("path is required")
			}
			if err := fs.writeFile(p, content); err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(fmt.Sprintf("已写入 %s（%d 字节）", p, len(content)))
		})

	s.RegisterTool("edit_file",
		"对文件做精确的文本替换（支持多处编辑）。dryRun=true 时只返回预览不落盘。",
		schema(`"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["oldText","newText"]}},"dryRun":{"type":"boolean"}`, "path", "edits"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			if p == "" {
				return errR("path is required")
			}
			var edits []fileEdit
			switch v := args["edits"].(type) {
			case []any:
				for _, e := range v {
					m, ok := e.(map[string]any)
					if !ok {
						return errR("每个 edit 必须是对象 {oldText, newText}")
					}
					edits = append(edits, fileEdit{Old: argString(m, "oldText"), New: argString(m, "newText")})
				}
			case []map[string]any:
				for _, m := range v {
					edits = append(edits, fileEdit{Old: argString(m, "oldText"), New: argString(m, "newText")})
				}
			default:
				return errR("edits 必须是数组")
			}
			if len(edits) == 0 {
				return errR("edits is required")
			}
			out, err := fs.editFile(p, edits, argBool(args, "dryRun"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(out)
		})

	s.RegisterTool("create_directory",
		"创建目录（父目录递归创建）。",
		schema(`"path":{"type":"string"}`, "path"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			if p == "" {
				return errR("path is required")
			}
			abs, err := fs.resolve(p)
			if err != nil {
				return errR("%v", err)
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return errR("创建目录失败: %v", err)
			}
			return mcp.TextResult(fmt.Sprintf("已创建目录 %s", abs))
		})

	s.RegisterTool("list_directory",
		"列出目录内容（目录在前、文件在后，含大小与修改时间）。",
		schema(`"path":{"type":"string"}`, "path"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			if p == "" {
				p = "."
			}
			out, err := fs.listDirectory(p)
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(out)
		})

	s.RegisterTool("directory_tree",
		"递归输出目录树（默认深度 3，可用 depth 调整）。",
		schema(`"path":{"type":"string"},"depth":{"type":"integer","description":"最大递归深度，默认 3"}`, "path"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			if p == "" {
				p = "."
			}
			depth := argInt(args, "depth")
			if depth <= 0 {
				depth = 3
			}
			out, err := fs.directoryTree(p, depth)
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(out)
		})

	s.RegisterTool("move_file",
		"移动或重命名文件/目录。",
		schema(`"source":{"type":"string"},"destination":{"type":"string"}`, "source", "destination"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			src, dst := argString(args, "source"), argString(args, "destination")
			if src == "" || dst == "" {
				return errR("source 与 destination 必填")
			}
			a, err := fs.resolve(src)
			if err != nil {
				return errR("%v", err)
			}
			b, err := fs.resolve(dst)
			if err != nil {
				return errR("%v", err)
			}
			if err := os.Rename(a, b); err != nil {
				return errR("移动失败: %v", err)
			}
			return mcp.TextResult(fmt.Sprintf("已移动 %s → %s", a, b))
		})

	s.RegisterTool("search_files",
		"在目录下按 glob 模式搜索文件名（如 \"*.go\"），支持排除模式。",
		schema(`"path":{"type":"string"},"pattern":{"type":"string","description":"glob 模式，如 *.go 或 *test*"},"excludePatterns":{"type":"array","items":{"type":"string"}}`, "path", "pattern"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			if p == "" {
				p = "."
			}
			pattern := argString(args, "pattern")
			if pattern == "" {
				return errR("pattern is required")
			}
			out, err := fs.searchFiles(p, pattern, argStrings(args, "excludePatterns"))
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(out)
		})

	s.RegisterTool("get_file_info",
		"返回文件的元信息（大小、修改时间、是否目录、权限等）。",
		schema(`"path":{"type":"string"}`, "path"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			p := argString(args, "path")
			if p == "" {
				return errR("path is required")
			}
			info, err := fs.fileInfo(p)
			if err != nil {
				return errR("%v", err)
			}
			return mcp.TextResult(info)
		})

	s.RegisterTool("list_allowed_directories",
		"列出当前允许访问的根目录列表。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult("允许访问的目录：\n" + strings.Join(fs.allowed, "\n"))
		})
}

type filesystem struct {
	allowed []string
}

type fileEdit struct {
	Old string
	New string
}

// resolve 把路径解析为绝对路径并确保落在 allowed 内（防路径穿越 / 符号链接逃逸）。
func (f *filesystem) resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %v", err)
	}
	// 对已存在的部分做符号链接解析，防止 symlink 指向目录外
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else {
		// 目标不存在：解析最近存在的父目录
		parent := filepath.Dir(abs)
		if rp, e2 := filepath.EvalSymlinks(parent); e2 == nil {
			abs = filepath.Join(rp, filepath.Base(abs))
		}
	}
	for _, dir := range f.allowed {
		ad, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if ar, err := filepath.EvalSymlinks(ad); err == nil {
			ad = ar
		}
		rel, err := filepath.Rel(ad, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("路径 %s 不在允许的目录内（%s）", p, strings.Join(f.allowed, ", "))
}

func (f *filesystem) readFile(p string) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("读取失败: %v", err)
	}
	// 二进制探测：含 NUL 字节视为二进制，返回提示而非乱码
	if strings.ContainsRune(string(data), '\x00') {
		return "", fmt.Errorf("%s 是二进制文件，无法以文本读取", abs)
	}
	return addLineNumbers(string(data)), nil
}

func (f *filesystem) writeFile(p, content string) error {
	abs, err := f.resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("创建父目录失败: %v", err)
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func (f *filesystem) editFile(p string, edits []fileEdit, dryRun bool) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("读取失败: %v", err)
	}
	text := string(data)
	var applied []string
	for i, e := range edits {
		if !strings.Contains(text, e.Old) {
			return "", fmt.Errorf("第 %d 处编辑的 oldText 未在文件中找到", i+1)
		}
		text = strings.Replace(text, e.Old, e.New, 1)
		applied = append(applied, fmt.Sprintf("%d: %q → %q", i+1, e.Old, e.New))
	}
	if dryRun {
		return "预览（未写入）\n" + strings.Join(applied, "\n") + "\n\n--- 编辑后内容 ---\n" + text, nil
	}
	if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("写入失败: %v", err)
	}
	return "已应用 " + strings.Join(applied, "; "), nil
}

func (f *filesystem) listDirectory(p string) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("读取目录失败: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})
	var b strings.Builder
	fmt.Fprintf(&b, "目录 %s 共 %d 项：\n", abs, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		kind := "文件"
		if e.IsDir() {
			kind = "目录"
		}
		fmt.Fprintf(&b, "%-6s %10d  %s  %s\n", kind, info.Size(), info.ModTime().Format("2006-01-02 15:04"), e.Name())
	}
	return b.String(), nil
}

func (f *filesystem) directoryTree(p string, depth int) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(abs + "\n")
	var walk func(dir string, prefix string, d int)
	walk = func(dir string, prefix string, d int) {
		if d > depth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool {
			di, dj := entries[i].IsDir(), entries[j].IsDir()
			if di != dj {
				return di
			}
			return entries[i].Name() < entries[j].Name()
		})
		for i, e := range entries {
			last := i == len(entries)-1
			conn := "├── "
			childPrefix := prefix + "│   "
			if last {
				conn = "└── "
				childPrefix = prefix + "    "
			}
			fmt.Fprintf(&b, "%s%s%s\n", prefix, conn, e.Name())
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()), childPrefix, d+1)
			}
		}
	}
	walk(abs, "", 1)
	return b.String(), nil
}

func (f *filesystem) searchFiles(p, pattern string, excludes []string) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	var matches []string
	err = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过无权限项
		}
		if path == abs {
			return nil
		}
		for _, ex := range excludes {
			if ok, _ := filepath.Match(ex, filepath.Base(path)); ok {
				return nil
			}
		}
		if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("搜索失败: %v", err)
	}
	if len(matches) == 0 {
		return fmt.Sprintf("在 %s 下未找到匹配 %q 的文件", abs, pattern), nil
	}
	sort.Strings(matches)
	return fmt.Sprintf("匹配 %q 共 %d 个：\n%s", pattern, len(matches), strings.Join(matches, "\n")), nil
}

func (f *filesystem) fileInfo(p string) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat 失败: %v", err)
	}
	kind := "文件"
	if info.IsDir() {
		kind = "目录"
	}
	return fmt.Sprintf("路径: %s\n类型: %s\n大小: %d 字节\n修改时间: %s\n权限: %s",
		abs, kind, info.Size(), info.ModTime().Format("2006-01-02 15:04:05"), info.Mode().String()), nil
}

func addLineNumbers(s string) string {
	if s == "" {
		return "(空文件)"
	}
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%6d│%s\n", i+1, ln)
	}
	return b.String()
}
