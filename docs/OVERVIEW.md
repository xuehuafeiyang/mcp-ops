# mcp-ops 功能概述

> 版本 0.3.0 ｜ Go 1.21+ ｜ 单二进制 `opsctl` ｜ 5,249 行 Go（含测试）

## 1. 一句话定位

**一个通用 MCP 客户端（host）+ 三个自研能力 Server**：把「SSH 到多台服务器执行命令」「查询/写入多种数据库」「抓取网页正文」封装成标准 MCP 工具，并**挂载任意外部 stdio MCP server**（npx 官方 server、playwright-mcp、chrome-devtools-mcp 等），供 Kiro / Claude Desktop / Cursor 等智能体调用；同时自带一个用自然语言驱动的交互客户端。

它不是一个包装脚本，而是三样东西的合体：

| 组成 | 说明 |
|---|---|
| **MCP 协议层** | 自研 JSON-RPC 2.0 + Streamable HTTP + stdio 双传输，server 端与 client 端都是自己写的 |
| **三个能力 Server** | SSH、数据库、网页抓取，各自独立成 MCP Server（也可合并为内置能力） |
| **一个通用 MCP 客户端（host）** | 接 LLM（OpenAI 兼容 / Anthropic 原生），跑 Agent 循环；工具来源三实现——HTTP / stdio 子进程 / 进程内，统一 `ToolServer` 接口 |

## 2. 解决什么问题

日常运维排查的典型链式动作，过去要在 4 个工具间来回切：

```
登录跳板机 → ssh 到目标机看磁盘/日志 → 开数据库客户端查数据 → 复制报错去搜索引擎查文档
```

现在一句话交给智能体：**"看下 web1 磁盘和内存，再查 main-mysql 的 users 表有多少行，顺便查下这个报错什么意思"**——模型自己编排工具完成，你只看结论。而 `mcp_servers` 配置又让它能「顺便」调用文件搜索、知识图谱记忆、浏览器自动化等社区工具，**一个入口聚合所有能力**。

价值不在"能连"，而在**统一入口 + 模型自主编排 + 权限可控**。

## 3. 能力矩阵

| 能力 | 覆盖范围 | 关键实现 |
|---|---|---|
| **SSH 远程执行** | 任意多台主机 | 长连接池 + keepalive(30s) + 断线自动重连；私钥 / 密码 / ssh-agent 三种认证 |
| **数据库** | MySQL、TiDB、PostgreSQL、达梦 DM、金仓 KingbaseES、瀚高 HighGo、SQLite、MongoDB、Redis、Elasticsearch | 统一 `Driver` 抽象（Query / Execute / Close）；达梦走官方 dm 驱动（Linux/Windows），金仓/瀚高复用 PG 协议，TiDB 复用 MySQL 协议，SQLite 走纯 Go 驱动 |
| **内网库接入** | 经 SSH 端口转发 | `via_ssh` 字段，自动建本地隧道，无需手工 `ssh -L` |
| **网页抓取** | 任意 HTTP(S) 页面 | Readability 正文提取 + HTML→Markdown、robots.txt、三层 SSRF 防护、分页续读、5 分钟缓存 |
| **Postman 测试** | 本地 Collection v2.1 + 环境文件 | Go 原生解析、`{{变量}}` 迭代替换、goja JS 断言（pm.test/pm.expect）、测试报告 |
| **LLM 接入** | OpenAI 兼容（DeepSeek/Qwen/GLM/Kimi/Ollama/vLLM）+ Anthropic 原生 | `Provider` 接口 + 双适配器，工具调用协议各自适配 |
| **智能体接入** | 任何 MCP 客户端 | stdio（客户端拉起）或 Streamable HTTP（常驻服务） |
| **外部 MCP server 挂载** | 任意 stdio server（npx 官方 / 全局命令） | `mcp_servers` 段拉起子进程，握手后工具合并；同名自动改名 |

**不覆盖**（明确边界）：SQL Server / Oracle / ClickHouse、SSH 跳板机（ProxyJump）、JS 渲染的 SPA 页面、SOCKS/HTTP 代理出网。

## 4. 内置六个工具 + 外部工具

内置工具 6 个；`mcp_servers` 挂载的外部 server 工具动态合并进来（同名自动加 `<name>__` 前缀）。stdio 模式全部注册；HTTP 模式按 Server 拆分（ssh-server 2 个、db-server 3 个、web-server 1 个）。

| 工具 | 归属 | 参数 | 作用 |
|---|---|---|---|
| `list_ssh_hosts` | ssh | — | 列出已配置主机及各自白名单摘要 |
| `ssh_run` | ssh | `host`, `command` | 执行命令，返回 exit_code + 输出（截断 64 KB），超时发 SIGKILL |
| `list_databases` | db | — | 列出已配置数据库（类型、是否走隧道） |
| `db_query` | db | `database`, `statement`, `params?` | 只读查询；Redis 侧限读命令白名单 |
| `db_execute` | db | `database`, `statement`, `params?` | 写操作 / DDL / 任意 Redis 命令 |
| `fetch_url` | web | `url`, `max_length`, `start_index`, `raw` | 抓网页正文转 Markdown，支持分页续读 |

**外部 server 示例**（`examples/mcp-servers.example.yaml` 含完整 8 个）：filesystem（文件读写搜索）、memory（知识图谱）、sequential-thinking（推理）、everything、fetch、git、playwright、chrome-devtools。

**四种数据库的 `statement` 写法不同**（这是最容易踩的坑）：

```jsonc
// SQL（MySQL/TiDB/PG/达梦/金仓/瀚高/SQLite）：原生 SQL，可选 ? 占位参数（PG 家族自动改写为 $1）
{"database":"main-mysql","statement":"SELECT * FROM users WHERE id = ?","params":["42"]}

// MongoDB：JSON 操作描述符
{"database":"main-mongo","statement":"{\"op\":\"find\",\"collection\":\"users\",\"filter\":{\"age\":{\"$gt\":18}},\"limit\":10}"}

// Redis：命令行字符串
{"database":"cache-redis","statement":"HGETALL user:1001"}

// Elasticsearch：method + path + body
{"database":"logs-es","statement":"{\"method\":\"POST\",\"path\":\"/logs/_search\",\"body\":{\"query\":{\"match_all\":{}}}}"}
```

## 5. 架构与运行方式

```
                      ┌────────────────────────────────┐
   你 / 智能体 ──────▶ │ opsctl host（通用 MCP 客户端）  │
                      │  Agent 循环 + 终端 REPL         │
                      │  LLM: OpenAI 兼容 / Anthropic   │
                      └───────┬──────────────┬─────────┘
                内置（进程内注册）      外部 server（stdio 子进程拉起）
       ┌──────────┼──────────┐      ┌────────┼────────┐
   SSH │ 数据库 │ 网页抓取   │      │ npx 官方 server    │
   连接池│10种驱动│ fetch_url  │      │ filesystem/memory  │
       └─────────────────────┘      │ playwright-mcp     │
            │           │            └────────┬─────────┘
       SSH 服务器  MySQL/PG/...          任意 MCP 工具
```

三种跑法：

| 方式 | 命令 | 场景 | 常驻进程 |
|---|---|---|---|
| 自带 REPL | `opsctl host -config ...`（内置 + 外部 server 自动拉起） | 自己在命令行里用，多轮对话 | 否 |
| **stdio**（智能体首选） | `opsctl stdio -config ...` | Kiro / Claude Desktop / Cursor 自动拉起 | 否 |
| 远程 HTTP | `opsctl ssh-server` / `db-server` / `web-server` | 多客户端共享连接池、服务部署在远端 | 是 |

## 6. 配置：一份 YAML 管全部

```
llm:          provider + (openai | anthropic) 凭据 + max_turns + system_prompt
ssh_server:   listen + token
db_server:    listen + token
web_server:   listen + token
web_fetch:    user_agent / timeout / max_bytes / max_redirects /
              respect_robots / allow_private / default_headers / cache_ttl / max_length / max_concurrent
ssh_hosts[]:  name / host / port / user / auth{private_key|private_key_data|password|agent} /
              allowed_commands / command_timeout(默认 120s)
databases[]:  name / type / dsn 或 host+port+user+password+database /
              via_ssh / query_timeout(默认 30s)
mcp_servers[]: name / command / args / env / disabled   # 外部 stdio MCP server
```

- **热重载**：SIGHUP 或文件变更后 2 秒内生效，连接池与驱动全部重建
- **向后兼容**：不写 `web_fetch` 段照常工作，默认策略生效
- **明文凭据**：仅适合开发/测试，需 `chmod 600` + gitignore

## 7. 安全模型

**三层收放旋钮**（从外到内）：

| 层 | 机制 | 位置 |
|---|---|---|
| 智能体层 | `autoApprove` 白名单，决定哪些工具免确认 | Kiro/客户端 `mcp.json` |
| 命令策略层 | `allowed_commands` 前缀白名单（`["*"]` 全放行）+ 危险命令黑名单兜底 | `internal/sshsrv/policy.go` |
| 数据源层 | 数据库账号自身权限（只读账号 / Redis ACL） | 数据库侧 |

**SSH 白名单是逐段校验的**：`;` `&&` `||` `|` 换行切段后，每段都必须命中白名单，所以 `df -h; cat /etc/shadow` 会被拒。黑名单另有三条正则兜住 `curl ... | bash`、`echo/base64 ... | sh`、`sh <(...)`。

**网页抓取的三层 SSRF 防护**：URL 预检 → 每跳重定向重检 → `Dialer.Control` 校验真实连接 IP（堵住官方实现承认的 DNS rebinding 缺口）。默认拦截 loopback / 私网 / link-local / `169.254.169.254`。

**当前默认 = 零人工护栏**：`["*"]` + 全读写 + 无审计 + 不校验 host key。只适合「开发/测试、坏了能重建」。上生产前四件事：收窄白名单、收窄 `autoApprove`、换只读数据库账号、补审计日志。

## 8. 质量证据

| 类别 | 内容 |
|---|---|
| 单元测试 | 16 个 Test 函数 / 37 个子用例（`policy_test.go` 21 个锁命令策略，`fetch_test.go` 14 个锁抓取行为） |
| 真机验证 | `tests/live_test.py` 36 项，针对沙箱内真实 sshd + PostgreSQL + Redis 端到端跑通 |
| 协议验证 | 14 项：401 拦截、initialize 握手、session 强制、双端 tools/list、未知工具 -32602、超时结构化错误 |
| 发布验证 | 用发布包（非源码构建）实测 6 工具列举 + 真实网页抓取 + Redis 写入读回 |
| 静态检查 | `go vet ./...` 干净，`CGO_ENABLED=0` 静态链接，5 平台交叉编译 |

**真机测试揪出并修复的真实 bug**（模拟测试发现不了的）：

1. Redis `Do(ctx, cmd, []any{...})` 把切片当单参数 → 改可变参数展开
2. PG 用 `$1` 非 `?` → 加占位符自动改写（跳过字符串字面量与 `$$` 块）
3. 黑名单 `>/dev/` 误杀 `2>/dev/null` → 收窄为真实块设备
4. SSH 超时不关 session → 改 `Start` + `SIGKILL`，实测无孤儿进程
5. stdout/stderr 共用 buffer 并发写竞争 → 输出空白，分离后修复
6. `max_turns` 省略时为 0，Agent 循环一次不跑 → 兜底 30
7. 白名单只做整串匹配 → 补逐段校验
8. `"curl | sh"` 只匹配字面量 → 补正则

## 9. 关键设计决策（为什么这么做）

| 决策 | 选择 | 理由 |
|---|---|---|
| 形态 | 自研 MCP **客户端** + 自研 Server（双端） | 要程序化编排 LLM 与工具，现成 host 不能满足多服务器多库的连接管理 |
| 语言 | Go | 单二进制部署、`x/crypto/ssh` 成熟、并发模型干净；代价是 MCP SDK 生态弱于 TS，故协议层自研 |
| 传输 | Streamable HTTP 常驻 + stdio | HTTP 让连接池跨会话复用；stdio 让智能体零配置拉起 |
| 打包 | 单二进制多子命令 | 共享配置与类型定义，部署一个文件 |
| SSH 连接 | 每主机长连接 + keepalive | 批量巡检时避免每条命令 1–3 秒握手开销 |
| 数据库抽象 | `Driver` 三方法接口 | Query/Execute/Close 足够覆盖 SQL 与 NoSQL，加驱动不改上层 |
| 网页提取 | Readability + Markdown | 与 Firefox 阅读模式同源，剥离导航/广告的效果最好；不引入浏览器依赖 |
| 超时 | 分类可配 + 超时返回结构化错误 | SSH 120s / DB 30s / web 25s；错误文本引导模型自纠（加 LIMIT、缩范围） |
| 配置 | 静态 YAML + 热重载 | 不给模型动态注册目标的能力（安全面），改配置重启无成本 |
| 外部 server | stdio 子进程挂载，不重写 | filesystem/memory/playwright 等直接用官方实现，避免重复造轮子；统一 `ToolServer` 接口（HTTP/stdio/进程内）让三种来源无差别接入 |

## 10. 已知边界与路线图

**当前限制（按影响排序）**

1. 无写操作确认 / dry-run，无审计日志 —— 事后无法回答"模型干了什么"
2. host key 用 `InsecureIgnoreHostKey`，有中间人风险
3. 无跳板机（ProxyJump），只能连直连可达主机
4. 网页抓取不走系统代理（否则 SSRF 校验失效），必须经代理出网的环境不支持
5. Mongo 认证库固定 `admin`；PG 默认 `sslmode=disable`
6. HTTP 模式单 token 拥有全部工具权限，无工具级 ACL

**二版候选**：JSONL 审计、known_hosts 校验、ProxyJump、写操作 dry-run、工具级 ACL、SQL Server/ClickHouse 驱动、host 多会话并发。

## 11. 三分钟上手

```bash
# 1. 解包 + 准备配置
tar -xzf mcp-ops_0.2.0_linux_amd64.tar.gz
cd mcp-ops_0.2.0_linux_amd64
cp config.example.yaml config.yaml && chmod 600 config.yaml
vim config.yaml          # 填 LLM key、服务器、数据库

# 2A. 挂到智能体（Kiro）
cp examples/kiro-mcp.json <项目>/.kiro/settings/mcp.json
# 改 command 与 -config 为绝对路径，重启 Kiro

# 2B. 或用自己的 REPL
./opsctl ssh-server -config ./config.yaml &
./opsctl db-server  -config ./config.yaml &
./opsctl host       -config ./config.yaml
```

完整文档见 `docs/MANUAL.md`（33 页 PDF 版同目录）。
