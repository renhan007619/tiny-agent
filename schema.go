package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
)

// ============ 2. 工具转换层：Go 函数 <-> API 工具说明书 ============
//
// 对应 function_schema.py（get_function_schema）+ agent.py 第 2 块
// （make_schemas / make_function_map）。
//
// API 要求每个工具是 {name, description, input_schema} 的结构，
// 模型读到这些说明才知道"有哪些工具可用、每个工具要什么参数"。

// Tool 一个可调用工具：说明书（Name/Description/Schema）+ 执行器（fn）。
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // 完整 input_schema：{type, properties, required}
	fn          any            // func() 或 func(T) 形状的真实函数
}

// goTypeToJSONType 把 Go 反射类型映射成 JSON Schema 类型名。
// 对应 Python 版的 {int: "integer", str: "string", float: "number",
// bool: "boolean"} 映射表。
func goTypeToJSONType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "string"
	default:
		return "string" // 兜底（与 Python 版一致：未知类型默认 string）
	}
}

// structFieldName 取 struct 字段的 JSON 名字：优先 json tag，否则用 Go 字段名。
// （Go 版工具参数的"参数名"来源。）
func structFieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
		return strings.Split(tag, ",")[0]
	}
	return f.Name
}

// functionSchema 从函数签名生成 input_schema：{type, properties, required}。
// 支持两种形状：func()（无参，properties 为空）/ func(T)（T 为 struct，
// 每个字段是一个参数）。
func functionSchema(fn any) map[string]any {
	t := reflect.TypeOf(fn)
	properties := map[string]any{}
	required := []string{}
	if t.NumIn() == 1 { // 带参工具
		argT := t.In(0)
		for i := 0; i < argT.NumField(); i++ {
			f := argT.Field(i)
			name := structFieldName(f)
			prop := map[string]any{"type": goTypeToJSONType(f.Type)}
			if desc := f.Tag.Get("description"); desc != "" {
				prop["description"] = desc
			}
			properties[name] = prop
			// Go 参数没有默认值 -> 全部必填（与 Python 版无 default 即 required 一致）
			required = append(required, name)
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

// funcName 从函数指针拿函数名："main.getColor" -> "getColor"。
// Python 版用 f.__name__，Go 的 reflect 拿不到函数名，改用 runtime 取。
func funcName(fn any) string {
	full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	if i := strings.LastIndex(full, "."); i >= 0 {
		return full[i+1:]
	}
	return full
}

// newTool 把函数包装成 Tool：
// - 名字：自动提取（funcName）
// - description：显式传入（Python 版拿 docstring 当 description，
//   Go 的 reflect 读不到注释，显式传参是等价做法——注释里写清楚即可）
// - schema：从函数签名自动生成（functionSchema）
func newTool(fn any, description string) *Tool {
	return &Tool{
		Name:        funcName(fn),
		Description: description,
		Schema:      functionSchema(fn),
		fn:          fn,
	}
}

// execute 执行工具：把模型给的 JSON 参数反射进函数并调用。
// 无参函数直接调；带参函数把 input JSON unmarshal 进 struct 参数再调。
// 返回工具结果（any），由调用方决定如何序列化。
func (t *Tool) execute(input json.RawMessage) (any, error) {
	fv := reflect.ValueOf(t.fn)
	ft := fv.Type()
	switch ft.NumIn() {
	case 0:
		return normalizeOut(fv.Call(nil))
	case 1:
		argPtr := reflect.New(ft.In(0)) // *T
		if len(input) > 0 {
			if err := json.Unmarshal(input, argPtr.Interface()); err != nil {
				return nil, fmt.Errorf("tool %s 参数解析失败: %w", t.Name, err)
			}
		}
		return normalizeOut(fv.Call([]reflect.Value{argPtr.Elem()}))
	default:
		return nil, fmt.Errorf("tool %s 只支持 0 或 1 个参数", t.Name)
	}
}

// normalizeOut 处理函数返回值：
// 约定最后一个返回值若是 error 则检查；剩余值打包成 any
// （单值直接返回，多值返回 []any）。
func normalizeOut(out []reflect.Value) (any, error) {
	errType := reflect.TypeOf((*error)(nil)).Elem()
	n := len(out)
	if n >= 2 && out[n-1].Type().Implements(errType) {
		if !out[n-1].IsNil() {
			return nil, out[n-1].Interface().(error)
		}
		out = out[:n-1] // error 为 nil，剥掉
	}
	switch len(out) {
	case 0:
		return nil, nil
	case 1:
		return out[0].Interface(), nil
	default:
		vals := make([]any, len(out))
		for i, v := range out {
			vals[i] = v.Interface()
		}
		return vals, nil
	}
}

// makeToolMap 函数名 -> 工具的映射表。
// 对应 Python 的 make_function_map：模型返回的 tool_use 里只有工具名
// 字符串，要用名字找到真正要执行的 Tool。
func makeToolMap(tools []*Tool) map[string]*Tool {
	m := make(map[string]*Tool, len(tools))
	for _, t := range tools {
		m[t.Name] = t
	}
	return m
}

// stringify 把工具返回值转成字符串，塞进 tool_result 的 content。
// 对应 Python 版：
//   json.dumps(result, default=str) if isinstance(result, (dict, list))
//   else str(result)
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.RawMessage:
		return string(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}
