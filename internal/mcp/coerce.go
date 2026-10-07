package mcp

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// 数字形状：整串都得是合法的 JSON 数字，integer 还不许有小数和指数部分。
// 这两条正则四个语言必须一致——不挡的话各语言的字符串转数字各有各的宽松处，
// Go 的 ParseFloat 收 "inf"、Python 的 int() 收 "1_000"，同一份参数在不同
// 语言下会转出不一样的值。
var (
	intShape = regexp.MustCompile(`^[+-]?\d+$`)
	numShape = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
)

// coerceScalar 按 schema 声明的类型做保守强转，转不了就原样返回。
func coerceScalar(value any, want string) any {
	switch want {
	case "string":
		// bool 不能被当成数字转成 "true"
		switch v := value.(type) {
		case float64:
			if v == float64(int64(v)) {
				return strconv.FormatInt(int64(v), 10)
			}
			return strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case json.Number:
			return v.String()
		}
	case "integer":
		if s, ok := value.(string); ok {
			text := strings.TrimSpace(s)
			// "5.7" 配 integer 不做截断，原样交给 MCP 服务器报它的域内错误
			if !intShape.MatchString(text) {
				return value
			}
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n
			}
		}
	case "number":
		if s, ok := value.(string); ok {
			text := strings.TrimSpace(s)
			if !numShape.MatchString(text) {
				return value
			}
			if f, err := strconv.ParseFloat(text, 64); err == nil {
				return f
			}
		}
	case "boolean":
		if s, ok := value.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				return true
			case "false":
				return false
			}
		}
	}
	return value
}

// CoerceBySchema 按 JSON schema 递归修正模型给的参数 args。
//
// MCP 工具不在 tools[] 里的时候，参数是模型自由生成的，没有接口层的 schema 约束，偶尔
// 会写错 JSON 类型。这里的修正规则四个语言必须逐条一致：
//
//	schema 声明        模型给的               修正为
//	string            数字                   "8891"
//	integer / number  数字形字符串            5 / 5.0
//	boolean           "true" / "false"       true / false
//	array             单键对象且值是数组       拆出内层数组
//	array             逗号分隔字符串          按逗号切分去空白
//	object            对象                    按 properties 递归
//	array             数组                    按 items 递归每个元素
//
// 修正不了的原样往下传，交给 MCP 服务器报它自己的错——服务器的域内错误比本地
// 类型错误对模型更有指导性。
//
// schema 是 JSON Schema 形态的 map（即目标工具的 input_schema）。修正结果不是
// map（例如 schema 把顶层声明成 array，模型给的参数被拆成了数组）时返回原参数，
// 与源端调用点 `if fixed, ok := CoerceBySchema(inner, schema).(map[string]any); ok`
// 的取舍一致。
func CoerceBySchema(args map[string]any, schema map[string]any) map[string]any {
	fixed, ok := coerceValue(args, schema).(map[string]any)
	if !ok {
		return args
	}
	return fixed
}

// coerceValue 是 CoerceBySchema 的递归核心，签名为 any 以便处理 JSON 解码出来的
// 嵌套 schema（properties 的值、items）与任意层级的参数值。
func coerceValue(value any, schema any) any {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return value
	}
	want, _ := schemaMap["type"].(string)

	if want == "object" {
		obj, ok := value.(map[string]any)
		if !ok {
			return value
		}
		props, _ := schemaMap["properties"].(map[string]any)
		out := make(map[string]any, len(obj))
		for k, v := range obj {
			if sub, found := props[k]; found {
				out[k] = coerceValue(v, sub)
			} else {
				out[k] = v
			}
		}
		return out
	}

	if want == "array" {
		itemSchema := schemaMap["items"]
		// 模型常把数组包成 {"item": [...]} 这类单键对象
		if obj, isObj := value.(map[string]any); isObj && len(obj) == 1 {
			for _, inner := range obj {
				if arr, isArr := inner.([]any); isArr {
					value = arr
				}
			}
		} else if s, isStr := value.(string); isStr {
			// 也常拼成逗号分隔的字符串
			parts := strings.Split(s, ",")
			arr := make([]any, 0, len(parts))
			for _, p := range parts {
				if t := strings.TrimSpace(p); t != "" {
					arr = append(arr, t)
				}
			}
			value = arr
		}
		if arr, isArr := value.([]any); isArr {
			out := make([]any, len(arr))
			for i, item := range arr {
				out[i] = coerceValue(item, itemSchema)
			}
			return out
		}
		return value
	}

	if want != "" {
		return coerceScalar(value, want)
	}
	return value
}
