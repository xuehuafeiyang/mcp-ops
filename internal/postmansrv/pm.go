package postmansrv

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/dop251/goja"
)

// Response 是一次 HTTP 响应的归一化表示，供 pm.response 暴露给 JS。
type Response struct {
	Status       int
	StatusText   string
	ResponseTime int64
	Headers      map[string]string
	Body         []byte
}

// pmRuntime 封装一次请求的 JS 执行上下文（goja）。
type pmRuntime struct {
	vm      *goja.Runtime
	vars    *VarStore
	resp    *Response
	results []TestResult
}

// newPMRuntime 构造 goja 运行时并注入 pm 对象。
func newPMRuntime(vars *VarStore, resp *Response) *pmRuntime {
	p := &pmRuntime{
		vm:   goja.New(),
		vars: vars,
		resp: resp,
	}
	p.vm.SetFieldNameMapper(nil) // 使用默认命名，手动控制属性名
	p.inject()
	return p
}

// inject 注入 pm 对象：pm.test / pm.expect / pm.response / pm.environment 等。
func (p *pmRuntime) inject() {
	vm := p.vm
	pm := vm.NewObject()

	// pm.test(name, fn)
	pm.Set("test", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		fn, ok := goja.AssertFunction(call.Argument(1))
		if !ok {
			panic(vm.ToValue("pm.test: 第二个参数必须是函数"))
		}
		res := TestResult{Name: name, Passed: true}
		func() {
			defer func() {
				if e := recover(); e != nil {
					res.Passed = false
					res.Error = errString(e)
				}
			}()
			if _, err := fn(goja.Undefined()); err != nil {
				res.Passed = false
				res.Error = cleanErr(err.Error())
			}
		}()
		p.results = append(p.results, res)
		return goja.Undefined()
	})

	// pm.expect(value) -> 断言器
	pm.Set("expect", func(call goja.FunctionCall) goja.Value {
		return p.buildExpecter(call.Argument(0), false)
	})

	// pm.response：数据属性 + 断言链（to/be/have/not 等指向 status code 断言器）
	respObj := vm.NewObject()
	respObj.Set("code", p.resp.Status)
	respObj.Set("status", p.resp.StatusText)
	respObj.Set("responseTime", p.resp.ResponseTime)
	hdrs := vm.NewObject()
	for k, v := range p.resp.Headers {
		hdrs.Set(k, v)
	}
	respObj.Set("headers", hdrs)
	respObj.Set("text", func() string { return string(p.resp.Body) })
	respObj.Set("json", func() goja.Value {
		var v any
		if err := json.Unmarshal(p.resp.Body, &v); err != nil {
			return goja.Undefined()
		}
		return vm.ToValue(v)
	})
	// 断言链：pm.response.to.have.status(200) 中的 status 是断言方法，
	// 与 pm.response.status（状态文本）分离，避免命名冲突。
	expObj := p.buildExpecter(vm.ToValue(float64(p.resp.Status)), false).(*goja.Object)
	for _, w := range []string{"to", "be", "been", "is", "that", "which", "and", "have", "has", "of", "at"} {
		respObj.Set(w, expObj)
	}
	respObj.Set("not", expObj.Get("not"))
	pm.Set("response", respObj)

	// pm.environment / pm.variables / pm.collectionVariables
	pm.Set("environment", p.buildVarScope("environment"))
	pm.Set("variables", p.buildVarScope("local"))
	pm.Set("collectionVariables", p.buildVarScope("collection"))

	// pm.info（可选，测试脚本偶用）
	info := vm.NewObject()
	info.Set("name", "postman")
	pm.Set("info", info)

	vm.Set("pm", pm)
}

// buildVarScope 构造 {get, set, has} 的变量作用域对象。
func (p *pmRuntime) buildVarScope(scope string) *goja.Object {
	o := p.vm.NewObject()
	o.Set("get", func(call goja.FunctionCall) goja.Value {
		k := call.Argument(0).String()
		val, ok := p.vars.Get(k)
		if !ok {
			return goja.Undefined()
		}
		return p.vm.ToValue(val)
	})
	o.Set("set", func(call goja.FunctionCall) goja.Value {
		k := call.Argument(0).String()
		v := call.Argument(1).String()
		switch scope {
		case "environment":
			p.vars.SetEnvironment(k, v)
		case "collection":
			// collection 变量只读语义较弱，这里写 local 兜底
			p.vars.SetLocal(k, v)
		default:
			p.vars.SetLocal(k, v)
		}
		return goja.Undefined()
	})
	o.Set("has", func(call goja.FunctionCall) goja.Value {
		_, ok := p.vars.Get(call.Argument(0).String())
		return p.vm.ToValue(ok)
	})
	return o
}

// buildExpecter 构建 chai 风格断言器子集。
func (p *pmRuntime) buildExpecter(val goja.Value, negated bool) goja.Value {
	vm := p.vm
	obj := vm.NewObject()
	obj.Set("_val", val)
	obj.Set("_neg", negated)

	// 链式词：访问返回自身
	for _, w := range []string{"to", "be", "been", "is", "that", "which", "and", "have", "has", "of", "at"} {
		obj.Set(w, obj)
	}

	// not：返回取反的断言器（getter）
	_ = obj.DefineAccessorProperty("not", p.vm.ToValue(func(goja.FunctionCall) goja.Value {
		return p.buildExpecter(val, !negated)
	}), nil, goja.FLAG_FALSE, goja.FLAG_FALSE)

	// true / false / null / undefined / ok / empty / exist：getter 断言
	for _, name := range []string{"true", "false", "null", "undefined", "ok", "empty", "exist"} {
		n := name
		_ = obj.DefineAccessorProperty(n, p.vm.ToValue(func(goja.FunctionCall) goja.Value {
			p.assertBool(negated, val, n)
			return obj
		}), nil, goja.FLAG_FALSE, goja.FLAG_FALSE)
	}

	// 断言方法
	obj.Set("equal", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return vm.ToValue(val).StrictEquals(call.Argument(0)) }, "期望相等")
		return obj
	})
	obj.Set("eq", obj.Get("equal"))
	obj.Set("eql", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return jsonDeepEqual(val, call.Argument(0)) }, "期望深相等")
		return obj
	})
	obj.Set("status", obj.Get("equal")) // pm.response.to.have.status(200)
	obj.Set("include", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return includeCheck(val, call.Argument(0)) }, "期望包含")
		return obj
	})
	obj.Set("contain", obj.Get("include"))
	obj.Set("above", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return val.ToFloat() > call.Argument(0).ToFloat() }, "期望大于")
		return obj
	})
	obj.Set("greaterThan", obj.Get("above"))
	obj.Set("below", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return val.ToFloat() < call.Argument(0).ToFloat() }, "期望小于")
		return obj
	})
	obj.Set("lessThan", obj.Get("below"))
	obj.Set("least", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return val.ToFloat() >= call.Argument(0).ToFloat() }, "期望不小于")
		return obj
	})
	obj.Set("most", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return val.ToFloat() <= call.Argument(0).ToFloat() }, "期望不大于")
		return obj
	})
	obj.Set("oneOf", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return oneOfCheck(val, call.Argument(0)) }, "期望属于集合")
		return obj
	})
	obj.Set("property", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return propertyCheck(val, call.Argument(0).String()) }, "期望具有属性")
		return obj
	})
	obj.Set("lengthOf", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return lengthOf(val) == int(call.Argument(0).ToInteger()) }, "期望长度匹配")
		return obj
	})
	obj.Set("a", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return obj // 作为链式词
		}
		p.assertFn(negated, func() bool { return typeCheck(val, call.Argument(0).String()) }, "期望类型匹配")
		return obj
	})
	obj.Set("an", obj.Get("a"))
	obj.Set("match", func(call goja.FunctionCall) goja.Value {
		p.assertFn(negated, func() bool { return matchCheck(val, call.Argument(0).String()) }, "期望匹配正则")
		return obj
	})

	return obj
}

// assertFn 断言一个布尔条件，失败时 throw（由 pm.test 捕获）。
func (p *pmRuntime) assertFn(negated bool, cond func() bool, label string) {
	ok := cond()
	if negated {
		ok = !ok
	}
	if !ok {
		panic(p.vm.ToValue("断言失败：" + label))
	}
}

// assertBool 处理 true/false/null/undefined/ok/empty/exist 断言。
func (p *pmRuntime) assertBool(negated bool, val goja.Value, kind string) {
	ok := false
	switch kind {
	case "true":
		ok = val.ToBoolean()
	case "false":
		ok = !val.ToBoolean()
	case "null":
		ok = val == nil || goja.IsNull(val)
	case "undefined":
		ok = val == nil || goja.IsUndefined(val)
	case "ok":
		ok = val != nil && !goja.IsUndefined(val) && !goja.IsNull(val)
	case "exist":
		ok = val != nil && !goja.IsUndefined(val)
	case "empty":
		ok = isEmpty(val)
	}
	if negated {
		ok = !ok
	}
	if !ok {
		panic(p.vm.ToValue("断言失败：期望 " + kind))
	}
}

// errString 把 recover 到的值转成可读错误信息。
func errString(e any) string {
	if v, ok := e.(goja.Value); ok {
		return cleanErr(v.String())
	}
	return fmt.Sprintf("%v", e)
}

// cleanErr 去掉 goja 异常信息末尾的 Go 调用栈（"msg at ...(native)"）。
func cleanErr(e string) string {
	if i := strings.Index(e, " at "); i > 0 {
		return strings.TrimSpace(e[:i])
	}
	return e
}

// ---------- 比较辅助 ----------

func jsonDeepEqual(a, b goja.Value) bool {
	aj, _ := json.Marshal(a.Export())
	bj, _ := json.Marshal(b.Export())
	return string(aj) == string(bj)
}

func includeCheck(actual, expected goja.Value) bool {
	av := actual.Export()
	switch t := av.(type) {
	case string:
		return strings.Contains(t, expected.String())
	case []any:
		for _, x := range t {
			if reflect.DeepEqual(x, expected.Export()) {
				return true
			}
		}
	}
	return false
}

func oneOfCheck(actual, set goja.Value) bool {
	sv := set.Export()
	arr, ok := sv.([]any)
	if !ok {
		return false
	}
	for _, x := range arr {
		if reflect.DeepEqual(x, actual.Export()) {
			return true
		}
	}
	return false
}

func propertyCheck(actual goja.Value, name string) bool {
	av := actual.Export()
	switch t := av.(type) {
	case map[string]any:
		_, ok := t[name]
		return ok
	default:
		rv := reflect.ValueOf(t)
		if rv.Kind() == reflect.Map {
			return rv.MapIndex(reflect.ValueOf(name)).IsValid()
		}
	}
	return false
}

func lengthOf(actual goja.Value) int {
	av := actual.Export()
	switch t := av.(type) {
	case string:
		return len(t)
	case []any:
		return len(t)
	case map[string]any:
		return len(t)
	}
	return -1
}

func typeCheck(actual goja.Value, typ string) bool {
	switch typ {
	case "string":
		return actual.ExportType() != nil && reflect.TypeOf(actual.Export()).Kind() == reflect.String
	case "number":
		k := reflect.TypeOf(actual.Export()).Kind()
		return k == reflect.Int || k == reflect.Int64 || k == reflect.Float64 || k == reflect.Float32
	case "boolean":
		return reflect.TypeOf(actual.Export()).Kind() == reflect.Bool
	case "array":
		_, ok := actual.Export().([]any)
		return ok
	case "object":
		_, ok := actual.Export().(map[string]any)
		return ok
	case "null":
		return goja.IsNull(actual)
	case "undefined":
		return goja.IsUndefined(actual)
	case "function":
		_, ok := goja.AssertFunction(actual)
		return ok
	}
	return false
}

func matchCheck(actual goja.Value, pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(actual.String())
}

func isEmpty(actual goja.Value) bool {
	av := actual.Export()
	switch t := av.(type) {
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case nil:
		return true
	}
	return false
}

// RunScript 执行一段 test script（多行数组拼接），返回断言结果。
func (p *pmRuntime) RunScript(lines []string) []TestResult {
	if len(lines) == 0 {
		return nil
	}
	src := strings.Join(lines, "\n")
	_, err := p.vm.RunString(src)
	if err != nil {
		// 脚本级错误记为一个失败测试
		p.results = append(p.results, TestResult{Name: "script", Passed: false, Error: err.Error()})
	}
	return p.results
}
