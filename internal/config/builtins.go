package config

// Dir 返回 filesystem 允许访问的目录列表；为空时回退到 ["."]。
func (f *FilesystemConfig) Dirs() []string {
	if f == nil || len(f.AllowedDirectories) == 0 {
		return []string{"."}
	}
	return f.AllowedDirectories
}

// Store 返回 memory 的持久化文件路径；为空时回退到 "./.memory.json"。
func (m *MemoryConfig) Store() string {
	if m == nil || m.StoragePath == "" {
		return "./.memory.json"
	}
	return m.StoragePath
}

// Repo 返回 git 默认仓库；可能为空（表示要求每个工具显式传 repo_path）。
func (g *GitConfig) Repo() string {
	if g == nil {
		return ""
	}
	return g.DefaultRepo
}

// Chrome 返回浏览器可执行文件路径；为空时返回空字符串（由引擎自动探测）。
func (b *BrowserConfig) Chrome() string {
	if b == nil {
		return ""
	}
	return b.ExecutablePath
}

// IsHeadless 返回是否无头运行，默认 true。
func (b *BrowserConfig) IsHeadless() bool {
	if b == nil || b.Headless == nil {
		return true
	}
	return *b.Headless
}

// ProfileDir 返回浏览器用户数据目录（可复用登录态），可能为空。
func (b *BrowserConfig) ProfileDir() string {
	if b == nil {
		return ""
	}
	return b.UserDataDir
}
