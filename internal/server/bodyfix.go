// bodyfix.go 出站请求体的「安全修正」。
//
// 目前只有一件事：把过小的输出额度抬到下限。
//
// 为什么必须做（真号实测 2026-10-26，账号 7772869 / sn-sensenova-6-8-flash）：
//
//	max_tokens=16   → finish_reason=length，content 空，reasoning_tokens=16
//	max_tokens=512  → finish_reason=length，content 空，reasoning_tokens=512
//	max_tokens=768  → finish_reason=stop，content "收到"，reasoning_tokens=228
//	max_tokens=1024 → 同上，reasoning_tokens=206
//
// 也就是说**思考 token 算在 max_tokens 里**，客户端给 16/512 这种「只要几个字」
// 的额度时，思考会把预算吃光、正文为空。客户端看到的现象是「模型坏了」，
// 实际是预算不够。目录里的 reasoning 标记不可靠（sensenova 标 false 照样烧
// 200+ 思考 token），所以只能统一抬，不能按模型分档。
//
// 边界：没给字段不动（不发明额度）、非数字不动、非法 JSON 原样返回
// （上游客户端自己会报错，网关不该多一层改动）。
package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// defaultMinOutputTokens 是出站 max_tokens 的默认下限。
//
// 1024 而不是 512：实测 512 仍会被思考吃光。思考量随问题复杂度波动
// （简单问题 200-300，复杂问题更多），1024 留了 2-4 倍余量。
const defaultMinOutputTokens = 2048

// escalatedMaxTokens 是「思考吃光额度」时二次尝试的额度。
//
// 实测 sn-sensenova-6-8-flash 的思考量能到 4000+ 字（≈2000+ token），
// 1024 会被吃光；8192 给了足够余量，而它只是上限、不会凭空多花。
const escalatedMaxTokens = 8192

// stripTurnTitle 把请求体里的 title 字段删掉。
//
// 网关会把它转成 X-RACCOON-Title 头（用于积分流水对账），但**不能留在 body 里**：
// 真号实测 raccoon-8c4485 对未声明字段是严格校验的，多一个 title 直接
// HTTP 500 "Unexpected keyword argument 'title'"（sn-* 系列反而会忽略）。
// 删掉字段不改变语义，只让上游看到它认识的形状。
func stripTurnTitle(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["title"]; !ok {
		return body
	}
	delete(obj, "title")
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}

// raiseMaxTokens 把输出额度至少设到 target（用于「思考吃光」后的重试）。
//
// 只抬不降：客户端本来写了更大的额度就不要动它。
func raiseMaxTokens(body []byte, target int) []byte {
	if target <= 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	found, changed := false, false
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		raw, exists := obj[key]
		if !exists {
			continue
		}
		found = true
		if cur, ok := asInt64Any(raw); ok && cur >= int64(target) {
			continue
		}
		obj[key] = target
		changed = true
	}
	if !found {
		obj["max_tokens"] = target
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// liftMaxTokens 把不合法的输出额度抬到 floor。
//
// 三种协议的字段名都认：chat 的 max_tokens / max_completion_tokens、
// responses 的 max_output_tokens（messages 入口在转换时已折成 max_tokens）。
//
// 负数/0 也算「不合法」：客户端常拿 -1 表示「不限」，但上游直接 HTTP 400
// （2026-11-02 线上实测两种文案：中文「max_tokens参数非法：限制数值范围
// [1,131072]」、英文「The request parameter max_tokens is invalid or
// missing」）。按 0 个 token 理解更没意义，统一抬到 floor。
func liftMaxTokens(body []byte, floor int) []byte {
	if floor <= 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := obj[key].(float64)
		if !ok || v >= float64(floor) {
			continue
		}
		obj[key] = float64(floor)
		changed = true
	}
	if !changed {
		return body
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}

// stripTemperature 摘掉出站请求里的 temperature。
//
// 为什么（2026-11-02 线上实测）：上游对思考型模型（sn-kimi-k3）只接受
// temperature=1，客户端带 0.7 这类默认值会被直接 HTTP 400
// "invalid temperature: only 1 is allowed for this model"。当天 28 条失败里
// 16 条是这个，而且下游把 400 当可重试，3 分钟连打 16 次、给 5 个号各记了
// 一次失败。温度对思考型模型本来不生效，与其维护「哪些模型只认 1」的名单
// （上游上新模型就会漏），不如一律不外发，让上游用自己的默认值。
//
// 代价：支持温度的普通模型（raccoon-*）也会退回上游默认温度。要恢复按模型
// 区分时，把这里改成按 body 里的 model 前缀判断即可。
func stripTemperature(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["temperature"]; !ok {
		return body
	}
	delete(obj, "temperature")
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}

// maxTokensRangeRe 从上游的参数校验报错里取「允许的输出额度区间」。
//
// 同一件事上游有两种文案，都得认（2026-11-02 线上实测）：
//
//	英文：Range of max_tokens should be [1, 131072]
//	中文：max_tokens参数非法：限制数值范围[1,131072]
//
// 区间按模型/版本会变（目录里的 max_output_tokens 是 100000，与校验上限
// 并不一致），硬编码会在上游改限后失效，所以按报错原文取。
var maxTokensRangeRe = regexp.MustCompile(`max_tokens[^\[\]]{0,40}\[\s*(\d+)\s*,\s*(\d+)\s*\]`)

// maxTokensRangeFromError 返回报错里的额度区间；不是这条报错就返回 0, 0。
func maxTokensRangeFromError(msg string) (lo, hi int) {
	m := maxTokensRangeRe.FindStringSubmatch(strings.ToLower(msg))
	if len(m) != 3 {
		return 0, 0
	}
	lo, err1 := strconv.Atoi(m[1])
	hi, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || hi <= 0 {
		return 0, 0
	}
	return lo, hi
}

// clampMaxTokens 把输出额度收进 [lo, hi]。
//
// 两端都要收：线上两条 400 的原文都是这条报错，客户端填的可能是负数（当
// 「不限」用）而不是超上限——只收上界的话它们会照样 400 透给客户端。
// 低于下界时补到 defaultMinOutputTokens：给 1 个 token 对思考型模型等于
// 只回空正文（见 liftMaxTokens 的实测）。
//
// changed=false 表示这条 body 里没有可修的字段，调用方不该重试（否则会拿
// 同一份 body 再吃一次同样的 400）。
func clampMaxTokens(body []byte, lo, hi int) (out []byte, changed bool) {
	if hi <= 0 && lo <= 0 {
		return body, false
	}
	floor := lo
	if floor < defaultMinOutputTokens {
		floor = defaultMinOutputTokens
	}
	if hi > 0 && floor > hi {
		floor = hi
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := obj[key].(float64)
		if !ok {
			continue
		}
		switch {
		case hi > 0 && v > float64(hi):
			obj[key] = float64(hi)
			changed = true
		case v < float64(floor):
			obj[key] = float64(floor)
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return raw, true
}
