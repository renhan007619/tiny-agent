package interview

import (
	"strings"
	"testing"
)

// 一段"合格"的回答：够长（>20 字）、带数字（不会被代码预检扣 no_number）。
// 用它当默认底噪，测某个点时只改一处，其余不干扰。
const goodAnswer = "我们用了三台机器做负载均衡，QPS 大概 3000，每次请求延迟在 20 毫秒左右。"

// GradeFromRaw 是回放的核心：同一段原始判词，任何时候算出来的分必须一样。
// 这个测试就是那条"可复现"承诺的守门人。
func TestGradeFromRaw_ZeroHits(t *testing.T) {
	raw := JudgeOpen + `{"hits": []}` + JudgeClose

	sc, _, err := GradeFromRaw(raw, goodAnswer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if sc.Tier != TierA || sc.Total != 10 {
		t.Fatalf("零命中必须是 A 档 10 分，实际 %s/%d（hits=%v）", sc.Tier, sc.Total, sc.Hits)
	}
}

// 模型报的扣分项要能一路走到档位上。
func TestGradeFromRaw_ModelHits(t *testing.T) {
	cases := []struct {
		name  string
		hits  string // JSON 里的数组片段
		want  Tier
		score int
	}{
		{"没讲取舍", `["no_tradeoff"]`, TierB, 8},
		{"只报名词", `["name_only"]`, TierC, 6},
		{"答非所问", `["off_topic"]`, TierD, 4},
		{"核心说错", `["wrong_core"]`, TierE, 2},
		// 两条一起：1+1=2 格，A→C
		{"没取舍+没边界", `["no_tradeoff","no_boundary"]`, TierC, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := JudgeOpen + `{"hits": ` + c.hits + `}` + JudgeClose
			sc, _, err := GradeFromRaw(raw, goodAnswer)
			if err != nil {
				t.Fatalf("不该出错: %v", err)
			}
			if sc.Tier != c.want || sc.Total != c.score {
				t.Fatalf("想要 %s/%d，实际 %s/%d", c.want, c.score, sc.Tier, sc.Total)
			}
		})
	}
}

// 代码预检要能补上模型漏掉的项目，这是"两边都判"的意义。
func TestGradeFromRaw_CodePrecheck(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		wantHit string
	}{
		{
			name:    "通篇没数字",
			answer:  "我用 channel 做任务队列，生产者往里面塞任务，消费者取出来一个个处理掉。",
			wantHit: "no_number",
		},
		{
			name:    "短到等于没答",
			answer:  "不知道",
			wantHit: "too_short",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := JudgeOpen + `{"hits": []}` + JudgeClose
			sc, _, err := GradeFromRaw(raw, c.answer)
			if err != nil {
				t.Fatalf("不该出错: %v", err)
			}
			found := false
			for _, h := range sc.Hits {
				if h == c.wantHit {
					found = true
				}
			}
			if !found {
				t.Fatalf("代码预检应该补上 %s，实际命中 %v", c.wantHit, sc.Hits)
			}
		})
	}
}

// 模型和代码都报同一项时，不能重复扣分。
func TestGradeFromRaw_MergeNoDup(t *testing.T) {
	answer := "我用 channel 做任务队列，生产者塞任务，消费者取出来处理。"
	raw := JudgeOpen + `{"hits": ["no_number"]}` + JudgeClose

	sc, _, err := GradeFromRaw(raw, answer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	n := 0
	for _, h := range sc.Hits {
		if h == "no_number" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("no_number 应该只出现一次，实际 %d 次（hits=%v）", n, sc.Hits)
	}
	// 只扣 1 格：A→B
	if sc.Tier != TierB {
		t.Fatalf("只扣一格应该是 B 档，实际 %s", sc.Tier)
	}
}

// 【必记】depth 是"讲得有多深"的计量，不许影响分数。
// 这条守住的是整套确定性的根：同一段回答，depth 写 3 还是写 1，档位必须一样。
func TestGradeFromRaw_DepthMustNotAffectScore(t *testing.T) {
	sc1, _, err := GradeFromRaw(JudgeOpen+`{"hits": ["no_tradeoff"], "depth": 1}`+JudgeClose, goodAnswer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	sc2, _, err := GradeFromRaw(JudgeOpen+`{"hits": ["no_tradeoff"], "depth": 3}`+JudgeClose, goodAnswer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if sc1.Tier != sc2.Tier || sc1.Total != sc2.Total {
		t.Fatalf("depth 不该影响档位：depth=1 得 %s/%d，depth=3 得 %s/%d",
			sc1.Tier, sc1.Total, sc2.Tier, sc2.Total)
	}
	if sc1.Depth != 1 || sc2.Depth != 3 {
		t.Fatalf("depth 本身要被读进来：%d / %d", sc1.Depth, sc2.Depth)
	}
}

// 模型写越界的 depth，要静默夹回 0~3，不能报错、也不能透传。
func TestGradeFromRaw_DepthClamp(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`{"hits": [], "depth": 7}`, 3},
		{`{"hits": [], "depth": -1}`, 0},
		{`{"hits": [], "depth": 2}`, 2},
		{`{"hits": []}`, 0}, // 没填就是 0（未判定）
	}
	for _, c := range cases {
		sc, _, err := GradeFromRaw(JudgeOpen+c.in+JudgeClose, goodAnswer)
		if err != nil {
			t.Fatalf("%s 不该出错: %v", c.in, err)
		}
		if sc.Depth != c.want {
			t.Fatalf("%s：depth 想要 %d，实际 %d", c.in, c.want, sc.Depth)
		}
	}
}

// suspect 要去掉首尾空白；空字符串就是"没有可疑点"。
func TestGradeFromRaw_SuspectTrim(t *testing.T) {
	sc, _, err := GradeFromRaw(JudgeOpen+`{"hits": [], "suspect": "  数字像是编的  "}`+JudgeClose, goodAnswer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if sc.Suspect != "数字像是编的" {
		t.Fatalf("suspect 该去掉空格，实际 %q", sc.Suspect)
	}

	sc2, _, err := GradeFromRaw(JudgeOpen+`{"hits": []}`+JudgeClose, goodAnswer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if sc2.Suspect != "" {
		t.Fatalf("没填时应该是空串，实际 %q", sc2.Suspect)
	}
}

// 只有评语、没有判卷块：不算失败，但要打上 Partial 记号。
func TestGradeFromRaw_OnlyReviewIsPartial(t *testing.T) {
	raw := ReviewOpen + `{"comment": "答得偏浅", "weak_points": []}` + ReviewClose

	sc, rv, err := GradeFromRaw(raw, goodAnswer)
	if err != nil {
		t.Fatalf("只有评语不该报错: %v", err)
	}
	if sc == nil {
		t.Fatal("判卷结论应该补一个出来")
	}
	if !sc.Partial {
		t.Fatal("补出来的分数必须标 Partial，不能假装算干净了")
	}
	if rv == nil || rv.Comment != "答得偏浅" {
		t.Fatalf("评语应该拿到: %+v", rv)
	}
}

// 两块都没有 = 这条返回不能用，要让上层去重试。
func TestGradeFromRaw_Nothing(t *testing.T) {
	_, _, err := GradeFromRaw("我觉得这题答得还行吧", goodAnswer)
	if err != ErrNoGrade {
		t.Fatalf("应该报 ErrNoGrade，实际: %v", err)
	}
}

// 一条铁律：hits 永远是空切片而不是 nil。
// 它会被 JSON 化成 null，前端拿去遍历就崩。
func TestGradeFromRaw_HitsNeverNil(t *testing.T) {
	sc, _, err := GradeFromRaw(JudgeOpen+`{"hits": []}`+JudgeClose, goodAnswer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if sc.Hits == nil {
		t.Fatal("Hits 不能是 nil，要空切片")
	}
}

// 同一份输入跑两次，结果必须一模一样——这是整个回放功能成立的前提。
func TestGradeFromRaw_Deterministic(t *testing.T) {
	raw := JudgeOpen + `{"hits": ["name_only","no_number"], "depth": 2}` + JudgeClose
	answer := "我用 channel 做队列"

	a, _, err := GradeFromRaw(raw, answer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	b, _, err := GradeFromRaw(raw, answer)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if a.Tier != b.Tier || a.Total != b.Total || a.Depth != b.Depth {
		t.Fatalf("两次结果不一致：%+v vs %+v", a, b)
	}
	if strings.Join(a.Hits, ",") != strings.Join(b.Hits, ",") {
		t.Fatalf("命中项顺序也不该变：%v vs %v", a.Hits, b.Hits)
	}
}
