package postmansrv

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LoadCollection 读取并解析 Postman Collection v2.1 JSON 文件。
func LoadCollection(path string) (*Collection, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read collection: %w", err)
	}
	var c Collection
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse collection %s: %w", path, err)
	}
	if c.Info.Schema != "" && !strings.HasPrefix(c.Info.Schema, "https://schema.getpostman.com/json/collection/v2") {
		// 仍尝试按 v2.1 解析，宽松处理
	}
	return &c, nil
}

// LoadEnvironment 读取并解析 Postman environment JSON 文件。
func LoadEnvironment(path string) (*Environment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read environment: %w", err)
	}
	var e Environment
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("parse environment %s: %w", path, err)
	}
	return &e, nil
}

// VarStore 维护变量作用域：local > environment > collection > 动态变量。
type VarStore struct {
	collection  map[string]string
	environment map[string]string
	local       map[string]string
}

// NewVarStore 从 collection 与 environment 构造变量存储。
func NewVarStore(c *Collection, e *Environment) *VarStore {
	v := &VarStore{
		collection:  map[string]string{},
		environment: map[string]string{},
		local:       map[string]string{},
	}
	if c != nil {
		for _, x := range c.Variables {
			if x.Enabled != nil && !*x.Enabled {
				continue
			}
			v.collection[x.Key] = x.Value
		}
	}
	if e != nil {
		for _, x := range e.Values {
			if x.Enabled != nil && !*x.Enabled {
				continue
			}
			v.environment[x.Key] = x.Value
		}
	}
	return v
}

// SetLocal 设置请求级变量（供 test script 通过 pm.variables.set 使用）。
func (v *VarStore) SetLocal(k, val string) { v.local[k] = val }

// SetEnvironment 设置环境变量（pm.environment.set）。
func (v *VarStore) SetEnvironment(k, val string) { v.environment[k] = val }

// Get 按作用域优先级取值。
func (v *VarStore) Get(k string) (string, bool) {
	if val, ok := v.local[k]; ok {
		return val, true
	}
	if val, ok := v.environment[k]; ok {
		return val, true
	}
	if val, ok := v.collection[k]; ok {
		return val, true
	}
	return "", false
}

var varRe = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

// Replace 把字符串里的 {{变量}} 全部替换；未定义的变量保留原文。
// 迭代解析，支持嵌套变量（如 collection 变量值引用 environment 变量）。
// 内置动态变量：$guid/$randomUUID、$timestamp、$randomInt。
func (v *VarStore) Replace(s string) string {
	for i := 0; i < 10; i++ {
		if !strings.Contains(s, "{{") {
			break
		}
		next := varRe.ReplaceAllStringFunc(s, func(m string) string {
			key := varRe.FindStringSubmatch(m)[1]
			key = strings.TrimSpace(key)
			if val, ok := v.Get(key); ok {
				return val
			}
			if dv, ok := dynamicVar(key); ok {
				return dv
			}
			return m // 未定义，保留原文
		})
		if next == s {
			break // 无进展，避免死循环
		}
		s = next
	}
	return s
}

func dynamicVar(key string) (string, bool) {
	switch key {
	case "$guid", "$randomUUID":
		return newUUID(), true
	case "$timestamp":
		return strconv.FormatInt(time.Now().Unix(), 10), true
	case "$randomInt":
		return strconv.Itoa(int(time.Now().UnixNano() % 1000)), true
	default:
		return "", false
	}
}

// newUUID 生成一个随机 UUID v4 字符串（避免额外依赖）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Expand 对 RequestSpec 的所有可变部分做变量替换。
func (v *VarStore) Expand(spec *RequestSpec) {
	spec.Method = v.Replace(spec.Method)
	spec.URL = v.Replace(spec.URL)
	for k, val := range spec.Headers {
		spec.Headers[k] = v.Replace(val)
	}
	spec.Body = []byte(v.Replace(string(spec.Body)))
}
