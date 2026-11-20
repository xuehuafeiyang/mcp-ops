#!/usr/bin/env python3
"""通过 stdio 驱动 opsctl 的 MCP Server，测试全部命令放行功能"""
import json
import os
import subprocess
import sys
import threading
import queue

BIN = os.environ.get("OPSCTL_BIN", "../bin/opsctl")
CFG = os.environ.get("OPSCTL_CFG", "live-config.example.yaml")

proc = subprocess.Popen(
    [BIN, "stdio", "-config", CFG],
    stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1,
)

responses = {}
lock = threading.Lock()
cv = threading.Condition(lock)
next_id = 0


def reader():
    for line in proc.stdout:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        with cv:
            responses[msg.get("id")] = msg
            cv.notify_all()


threading.Thread(target=reader, daemon=True).start()


def rpc(method, params=None, expect_response=True):
    global next_id
    req = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        req["params"] = params
    if expect_response:
        next_id += 1
        req["id"] = next_id
    proc.stdin.write(json.dumps(req) + "\n")
    proc.stdin.flush()
    if not expect_response:
        return None
    with cv:
        ok = cv.wait_for(lambda: next_id in responses, timeout=60)
        if not ok:
            return {"timeout": True}
        return responses.pop(next_id)


def call(tool, **args):
    r = rpc("tools/call", {"name": tool, "arguments": args})
    if "error" in r:
        return False, f"RPC_ERROR {r['error']['message']}"
    res = r.get("result", {})
    text = "".join(c.get("text", "") for c in res.get("content", []))
    return not res.get("isError", False), text.replace("\n", " ")[:150]


results = []


def check(name, tool, expect_ok, **args):
    ok, text = call(tool, **args)
    passed = (ok == expect_ok)
    results.append((passed, name, ok, text))
    print(f"{'PASS' if passed else 'FAIL'} | {name:44s} | ok={ok} | {text[:100]}")


rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "test", "version": "1"}})
rpc("notifications/initialized", None, expect_response=False)

print("\n########## SSH：allowed_commands=[\"*\"] 全放行 ##########")
check("SSH 任意命令 whoami+uname (原白名单外)", "ssh_run", True, host="local", command="whoami && uname -sm")
check("SSH 多级管道/重定向写入", "ssh_run", True, host="local",
      command="mkdir -p /tmp/opsctl-live && echo hello-ops > /tmp/opsctl-live/a.txt && cat /tmp/opsctl-live/a.txt")
check("SSH 读取系统文件", "ssh_run", True, host="local", command="head -2 /etc/passwd")
check("SSH python 执行", "ssh_run", True, host="local", command="python3 -c \"print(6*7)\"")
check("SSH 包管理命令 (apt list --installed)", "ssh_run", True, host="local", command="apt list --installed 2>/dev/null | head -3")
check("SSH 危险命令 rm -rf 仍拦截", "ssh_run", False, host="local", command="rm -rf /tmp/opsctl-live")
check("SSH 危险命令 shutdown 仍拦截", "ssh_run", False, host="local", command="shutdown -h now")
check("SSH 超时 (5s 上限跑 sleep 8) 应 TIMEOUT", "ssh_run", False, host="local", command="sleep 8")

print("\n########## Redis：读写全放行 ##########")
check("Redis 写 SET", "db_execute", True, database="local-redis", statement="SET ops:greet hello-mcp")
check("Redis 读 GET", "db_query", True, database="local-redis", statement="GET ops:greet")
check("Redis 写 INCR", "db_execute", True, database="local-redis", statement="INCR ops:counter")
check("Redis 写 HSET", "db_execute", True, database="local-redis", statement="HSET ops:hash f1 v1 f2 v2")
check("Redis 读 HGETALL", "db_query", True, database="local-redis", statement="HGETALL ops:hash")
check("Redis 读 KEYS", "db_query", True, database="local-redis", statement="KEYS ops:*")
check("Redis db_query 执行写命令 (INCR) 应拒绝", "db_query", False, database="local-redis", statement="INCR ops:counter")
check("Redis 写 DEL", "db_execute", True, database="local-redis", statement="DEL ops:hash")

print("\n########## PostgreSQL：DDL/DML 全放行 ##########")
check("PG 查询版本", "db_query", True, database="local-pg", statement="SELECT version() AS v")
check("PG 建表 DDL", "db_execute", True, database="local-pg",
      statement="CREATE TABLE IF NOT EXISTS t_demo (id serial PRIMARY KEY, name text, score int)")
check("PG 插入 (参数化)", "db_execute", True, database="local-pg",
      statement="INSERT INTO t_demo (name, score) VALUES (?, ?)", params=["alice", "90"])
check("PG 批量插入", "db_execute", True, database="local-pg",
      statement="INSERT INTO t_demo (name, score) VALUES (?, ?), (?, ?)", params=["bob", "75", "carol", "88"])
check("PG 查询", "db_query", True, database="local-pg", statement="SELECT * FROM t_demo ORDER BY id")
check("PG UPDATE", "db_execute", True, database="local-pg", statement="UPDATE t_demo SET score = score + 5 WHERE name = ?", params=["bob"])
check("PG ALTER TABLE", "db_execute", True, database="local-pg", statement="ALTER TABLE t_demo ADD COLUMN note text")
check("PG db_query 跑 INSERT (SQL 无读限制)", "db_query", True, database="local-pg",
      statement="INSERT INTO t_demo (name, score, note) VALUES ('dave', 60, 'via db_query') RETURNING id")
check("PG 聚合查询", "db_query", True, database="local-pg", statement="SELECT count(*) AS n, avg(score)::int AS avg FROM t_demo")
check("PG 错误 SQL 返回结构化错误", "db_query", False, database="local-pg", statement="SELECT * FROM no_such_table_xyz")
check("PG 超时 (3s 上限跑 pg_sleep 6)", "db_query", False, database="local-pg", statement="SELECT pg_sleep(6)")
check("PG 删表 DROP", "db_execute", True, database="local-pg", statement="DROP TABLE IF EXISTS t_demo")

print("\n########## SSH 隧道：via_ssh 连同一个 PG ##########")
check("隧道库查询", "db_query", True, database="pg-via-ssh", statement="SELECT current_database() AS db, 1+1 AS two")
check("隧道库建表删表", "db_execute", True, database="pg-via-ssh",
      statement="CREATE TABLE t_tunnel AS SELECT generate_series(1,3) AS n")
check("隧道库读回", "db_query", True, database="pg-via-ssh", statement="SELECT sum(n) AS s FROM t_tunnel")
check("隧道库清理", "db_execute", True, database="pg-via-ssh", statement="DROP TABLE t_tunnel")

print("\n########## 边界 ##########")
check("不存在的库", "db_query", False, database="nope", statement="SELECT 1")
check("不存在的 SSH 主机", "ssh_run", False, host="nope", command="echo hi")
check("空命令被拒", "ssh_run", False, host="local", command="   ")

proc.stdin.close()
proc.wait(timeout=10)

total = len(results)
passed = sum(1 for r in results if r[0])
print(f"\n========== 合计 {passed}/{total} 通过 ==========")
sys.exit(0 if passed == total else 1)
