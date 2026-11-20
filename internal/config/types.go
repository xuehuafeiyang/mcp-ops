package config

import (
	"fmt"
	"time"
)

// Duration 支持 YAML 中 "30s"、"2m" 等字符串形式
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	LLM        LLMConfig       `yaml:"llm"`
	SSHServer  ServerConfig    `yaml:"ssh_server"`
	DBServer   ServerConfig    `yaml:"db_server"`
	WebServer  ServerConfig    `yaml:"web_server"`
	WebFetch   WebFetchConfig  `yaml:"web_fetch"`
	SSHHosts   []SSHHost       `yaml:"ssh_hosts"`
	Databases  []Database      `yaml:"databases"`
	MCPServers []MCPServer     `yaml:"mcp_servers"`
	Builtins   BuiltinsConfig  `yaml:"builtins"`
	Postman    PostmanConfig   `yaml:"postman"`
}

// BuiltinsConfig 控制 Go 原生内置的 8 个官方 server 替代实现。
// 这些能力原本通过 mcp_servers 段拉起外部 Node/Python 进程，
// 现在由 opsctl 单二进制原生提供，无需 npx / node / playwright 等外部依赖。
type BuiltinsConfig struct {
	Filesystem FilesystemConfig `yaml:"filesystem"`
	Memory     MemoryConfig     `yaml:"memory"`
	Git        GitConfig        `yaml:"git"`
	Browser    BrowserConfig    `yaml:"browser"`
}

// FilesystemConfig 对应官方 server-filesystem：限定可访问目录，防路径穿越。
type FilesystemConfig struct {
	AllowedDirectories []string `yaml:"allowed_directories"` // 默认 ["."]（工作目录）
}

// MemoryConfig 对应官方 server-memory：知识图谱持久化位置。
type MemoryConfig struct {
	StoragePath string `yaml:"storage_path"` // 默认 "./.memory.json"
}

// GitConfig 对应官方 server-git：默认仓库路径（工具可选覆盖）。
type GitConfig struct {
	DefaultRepo string `yaml:"default_repo"` // 默认仓库路径；为空时要求每个工具显式传 repo_path
}

// BrowserConfig 对应 playwright 浏览器工具的底层引擎配置（chromedp/CDP）。
type BrowserConfig struct {
	ExecutablePath string `yaml:"executable_path"` // 默认自动探测（google-chrome/chromium）
	Headless       *bool  `yaml:"headless"`        // 默认 true
	UserDataDir    string `yaml:"user_data_dir"`   // 可选：复用登录态
}

// MCPServer 定义一个外部 MCP Server（stdio 子进程）。
// host 会以 command + args 拉起该子进程，完成握手后合并其工具，
// 与内置的 SSH/DB/Web 工具一起供模型调用。
//
// 例子（官方 filesystem server）：
//
//	mcp_servers:
//	  - name: filesystem
//	    command: npx
//	    args: ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
type MCPServer struct {
	Name     string            `yaml:"name"`     // 展示名，工具冲突时用作前缀
	Command  string            `yaml:"command"`  // 可执行文件，例如 npx / playwright-mcp
	Args     []string          `yaml:"args"`     // 参数
	Env      map[string]string `yaml:"env"`      // 附加环境变量（可选）
	Disabled bool              `yaml:"disabled"` // 置 true 暂时停用
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

type LLMConfig struct {
	Provider     string         `yaml:"provider"` // "openai" | "anthropic"
	OpenAI       OpenAIConfig   `yaml:"openai"`
	Anthropic    AnthropicConfig `yaml:"anthropic"`
	MaxTurns     int            `yaml:"max_turns"`      // Agent 循环最大轮数
	SystemPrompt string         `yaml:"system_prompt"` // host 系统提示词
}

type OpenAIConfig struct {
	BaseURL string `yaml:"base_url"` // 例如 https://api.deepseek.com/v1
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

type AnthropicConfig struct {
	BaseURL string `yaml:"base_url"` // 例如 https://api.anthropic.com
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

type SSHAuth struct {
	PrivateKey     string `yaml:"private_key"`      // 私钥文件路径
	PrivateKeyData string `yaml:"private_key_data"` // 内嵌 PEM（优先级高于路径）
	Password       string `yaml:"password"`
	Agent          bool   `yaml:"agent"` // 复用本机 ssh-agent
}

type SSHHost struct {
	Name            string      `yaml:"name"`
	Host            string      `yaml:"host"`
	Port            int         `yaml:"port"`
	User            string      `yaml:"user"`
	Auth            SSHAuth     `yaml:"auth"`
	AllowedCommands []string    `yaml:"allowed_commands"` // 前缀白名单
	CommandTimeout  Duration    `yaml:"command_timeout"`  // 默认 120s
}

func (h *SSHHost) Timeout() time.Duration {
	if h.CommandTimeout == 0 {
		return 120 * time.Second
	}
	return h.CommandTimeout.D()
}

type Database struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"` // mysql | tidb | postgres | dm | kingbase | highgo | sqlite | mongodb | redis | elasticsearch
	DSN      string `yaml:"dsn"`  // SQL 类可直接写完整 DSN（不走隧道时）；sqlite 写文件路径

	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"` // 库名（redis 为 db 编号语义仍写数字字符串；sqlite 为文件路径，如 /data/app.db 或 :memory:）

	ViaSSH       string   `yaml:"via_ssh"`     // 经哪个 SSH host 建隧道
	QueryTimeout Duration `yaml:"query_timeout"`
}

func (d *Database) Timeout() time.Duration {
	if d.QueryTimeout == 0 {
		return 30 * time.Second
	}
	return d.QueryTimeout.D()
}

// PostmanConfig 本地 Postman 测试引擎配置：collection 与环境文件清单。
type PostmanConfig struct {
	Collections  []PostmanFile `yaml:"collections"`
	Environments []PostmanFile `yaml:"environments"`
	Timeout      Duration      `yaml:"timeout"` // 单请求超时，默认 30s
}

// PostmanFile 是 collection/environment 的命名文件引用。
type PostmanFile struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// TimeoutD 返回 Postman 请求超时（默认 30s）。
func (p *PostmanConfig) TimeoutD() time.Duration {
	if p == nil || p.Timeout == 0 {
		return 30 * time.Second
	}
	return p.Timeout.D()
}

// FindSSHHost 按名称查找 SSH 主机
func (c *Config) FindSSHHost(name string) *SSHHost {
	for i := range c.SSHHosts {
		if c.SSHHosts[i].Name == name {
			return &c.SSHHosts[i]
		}
	}
	return nil
}

// FindDatabase 按名称查找数据库
func (c *Config) FindDatabase(name string) *Database {
	for i := range c.Databases {
		if c.Databases[i].Name == name {
			return &c.Databases[i]
		}
	}
	return nil
}
