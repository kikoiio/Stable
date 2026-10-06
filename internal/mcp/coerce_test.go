package mcp

import (
	"reflect"
	"testing"
)

var testSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"issueId": map[string]any{"type": "string"},
		"limit":   map[string]any{"type": "integer"},
		"ratio":   map[string]any{"type": "number"},
		"flag":    map[string]any{"type": "boolean"},
		"labels":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"ports":   map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
		"config": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"replicas": map[string]any{"type": "integer"},
				"features": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		},
	},
}

// 强转契约：这七条四个语言必须逐条一致。
func TestCoerceBySchemaContract(t *testing.T) {
	cases := []struct {
		desc  string
		given map[string]any
		want  map[string]any
	}{
		{"string ← 整数", map[string]any{"issueId": float64(8891)}, map[string]any{"issueId": "8891"}},
		{"string ← 小数", map[string]any{"issueId": 1.5}, map[string]any{"issueId": "1.5"}},
		{"integer ← 数字串", map[string]any{"limit": "5"}, map[string]any{"limit": int64(5)}},
		{"number ← 数字串带空白", map[string]any{"ratio": " 1.5 "}, map[string]any{"ratio": 1.5}},
		{"boolean ← true", map[string]any{"flag": "true"}, map[string]any{"flag": true}},
		{"boolean ← 大写 FALSE", map[string]any{"flag": "FALSE"}, map[string]any{"flag": false}},
		{
			"array ← 单键对象拆包",
			map[string]any{"labels": map[string]any{"item": []any{"a", "b"}}},
			map[string]any{"labels": []any{"a", "b"}},
		},
		{
			"array ← 逗号串",
			map[string]any{"labels": "a, b"},
			map[string]any{"labels": []any{"a", "b"}},
		},
		{
			"array 按 items 递归",
			map[string]any{"ports": []any{"8080", "9090"}},
			map[string]any{"ports": []any{int64(8080), int64(9090)}},
		},
		{
			"object 按 properties 递归，嵌套层同样适用",
			map[string]any{"config": map[string]any{
				"replicas": "4",
				"features": map[string]any{"item": []any{"x"}},
			}},
			map[string]any{"config": map[string]any{
				"replicas": int64(4),
				"features": []any{"x"},
			}},
		},
	}
	for _, c := range cases {
		got := CoerceBySchema(c.given, testSchema)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: 得到 %#v，期望 %#v", c.desc, got, c.want)
		}
	}
}

func TestCoerceBySchemaLeavesThingsAlone(t *testing.T) {
	// bool 不能被当成数字转成字符串
	if got := CoerceBySchema(map[string]any{"issueId": true}, testSchema); got["issueId"] != true {
		t.Errorf("bool 不该被转成字符串，得到 %#v", got)
	}
	// 转不了的原样往下传，交给 MCP 服务器报它自己的错
	if got := CoerceBySchema(map[string]any{"limit": "many"}, testSchema); got["limit"] != "many" {
		t.Error("转不了的应原样保留")
	}
	// schema 里没有的键不动
	if got := CoerceBySchema(map[string]any{"extra": 1}, testSchema); got["extra"] != 1 {
		t.Error("未知键应原样保留")
	}
	// 空 schema 是 no-op
	if got := CoerceBySchema(map[string]any{"a": "1"}, map[string]any{}); got["a"] != "1" {
		t.Error("空 schema 不该改动参数")
	}
}

// 各语言的字符串转数字各有各的宽松处：Go 的 ParseFloat 收 inf 和科学计数法，
// Python 的 int() 收下划线。这些形状四个语言必须给出同一个结果。
func TestCoerceNumericShapeParity(t *testing.T) {
	cases := []struct {
		key  string
		in   string
		want any
	}{
		{"limit", "5", int64(5)},
		{"limit", "+5", int64(5)},
		{"limit", "5.7", "5.7"}, // integer 不做截断
		{"limit", "1_000", "1_000"},
		{"limit", "1e3", "1e3"},
		{"limit", "5abc", "5abc"},
		{"ratio", " 1.5 ", 1.5},
		{"ratio", "1e3", 1000.0}, // 科学计数法是合法 JSON 数字，收
		{"ratio", "inf", "inf"},
		{"ratio", "nan", "nan"},
	}
	for _, c := range cases {
		got := CoerceBySchema(map[string]any{c.key: c.in}, testSchema)[c.key]
		if got != c.want {
			t.Errorf("%s=%q 转成 %#v，期望 %#v", c.key, c.in, got, c.want)
		}
	}
}

// 拆包只认单键对象，多键的猜不出意图，原样传下去让服务器报错
func TestCoerceMultiKeyObjectForArrayLeftAlone(t *testing.T) {
	inner := map[string]any{"item": "metrics", "tracing": ""}
	got := CoerceBySchema(map[string]any{"labels": inner}, testSchema)["labels"]
	m, ok := got.(map[string]any)
	if !ok || len(m) != 2 || m["item"] != "metrics" {
		t.Errorf("多键对象应原样保留，得到 %#v", got)
	}
}
