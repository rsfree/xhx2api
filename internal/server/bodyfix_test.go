package server

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestLiftMaxTokens 覆盖真号实测出来的那组边界（2026-10-26，sn-sensenova-6-8-flash）：
// max_tokens=512 时思考吃满预算、正文回空，768 起才稳定出正文。
func TestLiftMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64 // 期望的字段值；0 = 字段不该出现
	}{
		{"16 抬到下限", `{"max_tokens":16}`, 1024},
		{"512 也抬（实测仍不够）", `{"max_tokens":512}`, 1024},
		{"768 也抬（宁稳勿省）", `{"max_tokens":768}`, 1024},
		{"给够了不动", `{"max_tokens":4096}`, 4096},
		{"等于下限不动", `{"max_tokens":1024}`, 1024},
		{"没给就不发明", `{}`, 0},
		{"0 当没给", `{"max_tokens":0}`, 0},
		// 负数不是合法额度：原样保留（不抬、不删），让上游自己报错。
		{"负数原样保留", `{"max_tokens":-1}`, -1},
		{"responses 字段名也认", `{"max_output_tokens":16}`, 1024},
		{"chat 的 max_completion_tokens 也认", `{"max_completion_tokens":16}`, 1024},
		{"字符串不认（不是数字就不动）", `{"max_tokens":"16"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := liftMaxTokens([]byte(tc.in), 1024)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("输出不是 JSON: %v", err)
			}
			var v float64
			for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
				if f, ok := got[k].(float64); ok {
					v = f
				}
			}
			if v != tc.want {
				t.Fatalf("出站额度 = %v, want %v（输出 %s）", v, tc.want, out)
			}
			// 其他字段必须原样保留（不能因为抬额度把 messages 弄丢）。
			if tc.in == `{"max_tokens":16}` {
				if _, hasModel := got["model"]; hasModel {
					t.Fatal("不该凭空加字段")
				}
			}
		})
	}
}

func TestLiftMaxTokensDisabled(t *testing.T) {
	in := `{"max_tokens":16}`
	if out := liftMaxTokens([]byte(in), 0); string(out) != in {
		t.Fatalf("floor=0 应当原样返回，got %s", out)
	}
}

func TestLiftMaxTokensKeepsInvalidBody(t *testing.T) {
	in := "not json"
	if out := liftMaxTokens([]byte(in), 1024); string(out) != in {
		t.Fatalf("非法 JSON 被改动了: %s", out)
	}
}

func TestLiftMaxTokensPreservesOtherFields(t *testing.T) {
	in := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"你好"}],"stream":true}`
	out := liftMaxTokens([]byte(in), 1024)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" || got["stream"] != true {
		t.Fatalf("其他字段被改坏: %s", out)
	}
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages 丢了: %s", out)
	}
	if got["max_tokens"].(float64) != 1024 {
		t.Fatalf("max_tokens 没抬: %v", got["max_tokens"])
	}
}

// TestRaiseMaxTokensOnlyRaises 守卫「加额只抬不降」。
//
// 重试用例里最容易写错的就是「无条件把 max_tokens 设成 8192」——
// 客户端本来要 32768，被降成 8192 会直接截断回答。
func TestRaiseMaxTokensOnlyRaises(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 期望 max_tokens 的值
	}{
		{"小额度抬到 8192", `{"max_tokens":1024}`, "8192"},
		{"已更大则不动", `{"max_tokens":32768}`, "32768"},
		{"没给字段就补上", `{"messages":[]}`, "8192"},
		{"max_output_tokens 也认", `{"max_output_tokens":512}`, "8192"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := raiseMaxTokens([]byte(tc.in), 8192)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出不是 JSON: %s", out)
			}
			got := obj["max_tokens"]
			if got == nil {
				got = obj["max_output_tokens"]
			}
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("%s → max_tokens=%v，期望 %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripTemperature 守「temperature 一律不外发」。
//
// 上游对 sn-kimi-k3 只接受 temperature=1，客户端带 0.7 直接 400；网关不做
// 逐模型白名单，统一摘掉（2026-11-02 线上 57% 的失败来自这一条）。
func TestStripTemperature(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool // 输出里是否还带 temperature
	}{
		{"0.7 摘掉", `{"model":"sn-kimi-k3","temperature":0.7}`, false},
		{"0 也摘掉（不判断值）", `{"temperature":0}`, false},
		{"1 也摘掉（统一形状）", `{"temperature":1}`, false},
		{"没给就不动", `{"model":"m"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := stripTemperature([]byte(tc.in))
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("输出不是 JSON: %s", out)
			}
			if _, ok := got["temperature"]; ok != tc.want {
				t.Fatalf("temperature 还在: %s", out)
			}
		})
	}
	// 其他字段不能丢：摘 temperature 不能把 messages/model 一起弄没。
	in := `{"model":"m","temperature":0.7,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	out := stripTemperature([]byte(in))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" || got["stream"] != true {
		t.Fatalf("其他字段被改坏: %s", out)
	}
	if msgs, ok := got["messages"].([]any); !ok || len(msgs) != 1 {
		t.Fatalf("messages 丢了: %s", out)
	}
	// 非法 JSON 原样返回，网关不该多一层改动。
	if bad := "not json"; string(stripTemperature([]byte(bad))) != bad {
		t.Fatal("非法 JSON 被改动了")
	}
}

// TestMaxTokensRangeFromError 用的是线上原样报错（含前缀与 request_id）。
func TestMaxTokensRangeFromError(t *testing.T) {
	cases := []struct {
		msg    string
		lo, hi int
	}{
		{"upstream client (http 400): Range of max_tokens should be [1, 131072] (type: invalid_request_error) (request_id: chatcmpl-5f568a)", 1, 131072},
		{"Range of max_tokens should be [1, 8192]", 1, 8192},
		{"range of max_tokens should be [ 2 , 4096 ]", 2, 4096},
		{"invalid temperature: only 1 is allowed for this model", 0, 0},
		{"", 0, 0},
	}
	for _, tc := range cases {
		lo, hi := maxTokensRangeFromError(tc.msg)
		if lo != tc.lo || hi != tc.hi {
			t.Fatalf("%q → [%d, %d], want [%d, %d]", tc.msg, lo, hi, tc.lo, tc.hi)
		}
	}
}

// TestClampMaxTokens 守「两端都收、区间内不动」。
//
// 负数/0 是客户端当「不限」用的写法，只收上界的话它们会照样 400；收到下限
// 时补到 defaultMinOutputTokens（给 1 个 token 等于空正文）。
func TestClampMaxTokens(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		lo, hi  int
		want    string
		changed bool
	}{
		{"超上限收到上限", `{"max_tokens":200000}`, 1, 131072, "131072", true},
		{"负数补到下限", `{"max_tokens":-1}`, 1, 131072, "2048", true},
		{"0 也补到下限", `{"max_tokens":0}`, 1, 131072, "2048", true},
		{"等于上限不动", `{"max_tokens":131072}`, 1, 131072, "131072", false},
		{"区间内不动", `{"max_tokens":4096}`, 1, 131072, "4096", false},
		{"没字段不动", `{"messages":[]}`, 1, 131072, "0", false},
		{"max_output_tokens 也收", `{"max_output_tokens":200000}`, 1, 131072, "131072", true},
		{"上限比下限小时贴上限", `{"max_tokens":5000}`, 1, 1024, "1024", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := clampMaxTokens([]byte(tc.in), tc.lo, tc.hi)
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出不是 JSON: %s", out)
			}
			var v float64
			for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
				if f, ok := obj[k].(float64); ok {
					v = f
				}
			}
			if fmt.Sprint(v) != tc.want {
				t.Fatalf("%s → %v, want %s", tc.in, v, tc.want)
			}
		})
	}
}
