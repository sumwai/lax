package plugin_test

import (
	"encoding/json"
	"testing"

	"lax/internal/plugin"
)

// TestValueNodeShapes 钉住节点的合法与非法形状：map 是修饰键，来源键最多一个，
// 常量 null 能被表达。
func TestValueNodeShapes(t *testing.T) {
	valid := []struct {
		raw  string
		want func(plugin.Value) bool
		note string
	}{
		{
			raw: `{"input":"format","map":{"pcm":"pcm16","mp3":"mp3"}}`,
			want: func(v plugin.Value) bool {
				return v.Input == "format" && v.Map["pcm"] == "pcm16"
			},
			note: "map 按 input 取值查表",
		},
		{
			raw: `{"option":"region","map":{"SGP":"sgp"},"default":"sgp"}`,
			want: func(v plugin.Value) bool {
				return v.Option == "region" && v.HasDefault && v.Default == "sgp"
			},
			note: "map 可以配 option",
		},
		{
			raw: `{"const":null}`,
			want: func(v plugin.Value) bool {
				return v.HasConst && v.Const == nil
			},
			note: "常量 null",
		},
		{
			raw: `{"input":"voice","omit_if_empty":true}`,
			want: func(v plugin.Value) bool {
				return v.Input == "voice" && v.OmitIfEmpty
			},
			note: "省略空字段",
		},
	}
	for _, c := range valid {
		var v plugin.Value
		if err := json.Unmarshal([]byte(c.raw), &v); err != nil {
			t.Fatalf("%s：期望合法，得到错误 %v", c.raw, err)
		}
		if !c.want(v) {
			t.Fatalf("%s：解析结果不符合期望（%s）：%+v", c.raw, c.note, v)
		}
	}

	invalid := []string{
		`{"input":"a","const":"b"}`,
		`{"map":{"a":"b"}}`,
		`{"const":"a","map":{"a":"b"}}`,
		`{"input":"a","nope":1}`,
	}
	for _, raw := range invalid {
		var v plugin.Value
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			t.Fatalf("%s：期望被拒绝，却解析成功", raw)
		}
	}
}
