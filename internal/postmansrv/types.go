package postmansrv

// 本包实现一个本地 Postman 测试引擎：解析 Postman Collection v2.1 JSON
// 与 environment 文件，执行请求、替换 {{变量}}、运行 test script（goja JS），
// 并产出通过/失败统计。不依赖 Postman 账号或云端 API。

// Collection 是 Postman Collection v2.1 的顶层结构。
type Collection struct {
	Info struct {
		Name   string `json:"name"`
		Schema string `json:"schema"`
	} `json:"info"`
	Items     []Item     `json:"item"`
	Variables []Variable `json:"variable"`
}

// Item 是 collection 中的一个请求或一个文件夹（含嵌套 item）。
type Item struct {
	Name    string     `json:"name"`
	Request *Request   `json:"request"`
	Items   []Item     `json:"item"`
	Event   []Event    `json:"event"`
}

// IsFolder 判断是否为文件夹（无 request 但有嵌套 item）。
func (i *Item) IsFolder() bool { return i.Request == nil && len(i.Items) > 0 }

// Request 是一次 HTTP 请求。
type Request struct {
	Method string     `json:"method"`
	Header []KeyValue `json:"header"`
	URL    *URL       `json:"url"`
	Body   *Body      `json:"body"`
}

// URL 描述请求地址，可能是原始字符串或结构化 host/path/query。
type URL struct {
	Raw      string     `json:"raw"`
	Protocol string     `json:"protocol"`
	Host     []string   `json:"host"`
	Path     []string   `json:"path"`
	Query    []KeyValue `json:"query"`
	Variable []Variable `json:"variable"`
}

// String 返回可用的完整 URL（优先 raw，回退结构化拼接）。
func (u *URL) String() string {
	if u == nil {
		return ""
	}
	if u.Raw != "" {
		return u.Raw
	}
	var b []byte
	if u.Protocol != "" {
		b = append(b, u.Protocol...)
		b = append(b, "://"...)
	}
	for i, h := range u.Host {
		if i > 0 {
			b = append(b, '.')
		}
		b = append(b, h...)
	}
	for _, p := range u.Path {
		b = append(b, '/')
		b = append(b, p...)
	}
	if len(u.Query) > 0 {
		b = append(b, '?')
		for i, q := range u.Query {
			if i > 0 {
				b = append(b, '&')
			}
			b = append(b, q.Key...)
			b = append(b, '=')
			b = append(b, q.Value...)
		}
	}
	return string(b)
}

// Body 是请求体，支持 raw / urlencoded / formdata / json 等 mode。
type Body struct {
	Mode      string     `json:"mode"`
	Raw       string     `json:"raw"`
	URLEncoded []KeyValue `json:"urlencoded"`
	FormData  []KeyValue `json:"formdata"`
	Options   map[string]any `json:"options"`
}

// KeyValue 是 header/query/form 的一对键值。
type KeyValue struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Enabled *bool  `json:"enabled"`
	Type    string `json:"type"`
}

// EnabledOr 返回是否启用（缺省视为启用）。
func (kv KeyValue) EnabledOr() bool {
	return kv.Enabled == nil || *kv.Enabled
}

// Event 是 pre-request / test 脚本。
type Event struct {
	Listen string `json:"listen"`
	Script struct {
		Type string   `json:"type"`
		Exec []string `json:"exec"`
	} `json:"script"`
}

// Variable 是 collection/environment 变量。
type Variable struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Enabled *bool  `json:"enabled"`
	Type    string `json:"type"`
}

// Environment 是 Postman environment 文件。
type Environment struct {
	Name   string     `json:"name"`
	Values []Variable `json:"values"`
}

// RequestSpec 是执行一次请求所需的归一化参数。
type RequestSpec struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// TestResult 是一次断言（pm.test）的结果。
type TestResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Error   string `json:"error,omitempty"`
}

// RequestResult 是一个请求的完整执行结果。
type RequestResult struct {
	Name         string       `json:"name"`
	Method       string       `json:"method"`
	URL          string       `json:"url"`
	Status       int          `json:"status"`
	StatusText   string       `json:"status_text"`
	ResponseTime int64        `json:"response_time_ms"`
	BodyPreview  string       `json:"body_preview,omitempty"`
	Tests        []TestResult `json:"tests,omitempty"`
	Error        string       `json:"error,omitempty"`
	Passed       bool         `json:"passed"`
}

// CollectionResult 是一次 collection 运行的总报告。
type CollectionResult struct {
	Collection string          `json:"collection"`
	Total      int             `json:"total_requests"`
	Passed     int             `json:"passed"`
	Failed     int             `json:"failed"`
	Requests   []RequestResult `json:"requests"`
}
