package server

import "testing"

// 一次请求可能回多张图，签名者要按"首次写入、异源置 mixed、空值不覆盖"合并。
// 这三种情况各写一条：搞反任何一条，面板上的来源分布都会失真。
func TestMergeSigner(t *testing.T) {
	cases := []struct {
		name      string
		cur, next string
		want      string
	}{
		{"首次写入", "", "OpenAI Media Service", "OpenAI Media Service"},
		{"同源保持", "OpenAI Media Service", "OpenAI Media Service", "OpenAI Media Service"},
		{"异源置 mixed", "OpenAI Media Service", "Microsoft Corporation", "mixed"},
		{"mixed 不被覆盖", "mixed", "Microsoft Corporation", "mixed"},
		{"空值不覆盖已知来源", "Microsoft Corporation", "", "Microsoft Corporation"},
		{"两边都空", "", "", ""},
	}
	for _, c := range cases {
		if got := mergeSigner(c.cur, c.next); got != c.want {
			t.Errorf("%s: mergeSigner(%q, %q) = %q，想要 %q", c.name, c.cur, c.next, got, c.want)
		}
	}
}

// trace 为空时不能 panic：images 之外还有别的协议会走同一条收尾路径。
func TestNoteSignerNilTrace(t *testing.T) {
	noteSigner(nil, []byte("whatever"))
}
