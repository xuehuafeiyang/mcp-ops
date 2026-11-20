package dbsrv

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"mcp-ops/internal/config"
)

// redisDriver：Query 只允许读类命令；Execute 允许任意命令（全读写模型）。
// 命令以空格分隔传入，例如 "GET mykey"、"SCAN 0 MATCH user:* COUNT 100"。
type redisDriver struct {
	client *redis.Client
}

// 读类命令白名单（首 token，大小写不敏感）
var redisReadCmds = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "EXISTS": true, "TTL": true, "PTTL": true,
	"KEYS": true, "SCAN": true, "TYPE": true, "LRANGE": true, "LINDEX": true, "LLEN": true,
	"HGET": true, "HGETALL": true, "HMGET": true, "HKEYS": true, "HLEN": true, "HVALS": true,
	"SMEMBERS": true, "SISMEMBER": true, "SCARD": true, "SRANDMEMBER": true,
	"ZRANGE": true, "ZREVRANGE": true, "ZSCORE": true, "ZCARD": true, "ZRANK": true,
	"GETRANGE": true, "BITCOUNT": true, "OBJECT": true, "RANDOMKEY": true, "DBSIZE": true,
	"INFO": true, "PING": true, "TIME": true, "CONFIG": true, "CLIENT": true, "XLEN": true,
	"XRANGE": true, "XINFO": true, "GEOPOS": true, "GEODIST": true, "MEMORY": true, "SLOWLOG": true,
}

func openRedis(cfg *config.Database, addr string) (*redisDriver, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: cfg.Password,
		DB:       atoiDefault(cfg.Database, 0),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*1e9)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &redisDriver{client: client}, nil
}

func (d *redisDriver) Type() string { return "redis" }
func (d *redisDriver) Close()       { _ = d.client.Close() }

func (d *redisDriver) Query(ctx context.Context, q string, _ []string) (string, error) {
	parts := strings.Fields(q)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmd := strings.ToUpper(parts[0])
	if !redisReadCmds[cmd] {
		return "", fmt.Errorf("db_query only allows read commands; %q is not read-only, use db_execute instead", parts[0])
	}
	return d.do(ctx, parts)
}

func (d *redisDriver) Execute(ctx context.Context, q string, _ []string) (string, error) {
	parts := strings.Fields(q)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}
	return d.do(ctx, parts)
}

func (d *redisDriver) do(ctx context.Context, parts []string) (string, error) {
	// go-redis 的 Do 是可变参数：Do(ctx, cmd, arg1, arg2...)，不能把切片整体传入
	args := make([]any, len(parts))
	for i, p := range parts {
		args[i] = p
	}
	res, err := d.client.Do(ctx, args...).Result()
	if err == redis.Nil {
		return "(nil)", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", res), nil
}
