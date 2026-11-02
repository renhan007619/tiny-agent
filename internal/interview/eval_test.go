package interview

import (
	"errors"
	"testing"
)

// 一条坑都没踩 = A 档满分。
//
// 这是「天花板必须开着」的守门测试。
// 改造前 5 条写得最好的回答没一条能到 A——全都卡在 B。
// 只要这个测试挂了，就说明「它又在乱扣分」。
func TestParseGrade_NoHits(t *testing.T) {
	raw := JudgeOpen + `{"hits": []}` + JudgeClose

	s, _, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if s == nil {
		t.Fatal("判卷结论应该拿到")
	}
	if s.Tier != TierA {
		t.Fatalf("零命中必须是 A 档，实际: %s（命中: %v）", s.Tier, s.Hits)
	}
	if s.Total != 10 {
		t.Fatalf("A 档应该是 10 分，实际: %d", s.Total)
	}
}

// 两块都给：判卷结论和评语都该拿到。
func TestParseGrade_Both(t *testing.T) {
	raw := "判卷如下：\n" +
		JudgeOpen + "\n" +
		`{"hits": ["no_tradeoff"], "evidence": {"no_tradeoff": "只说了用了什么方案"}}` + "\n" +
		JudgeClose + "\n" +
		ReviewOpen + "\n" +
		`{"comment": "答得偏浅", "weak_points": [{"tag": "表达/只讲做了什么没讲为什么", "detail": "讲了用了什么方案，没讲为什么不用别的"}]}` + "\n" +
		ReviewClose

	s, r, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if s == nil || len(s.Hits) != 1 || s.Hits[0] != "no_tradeoff" {
		t.Fatalf("命中项不对: %+v", s)
	}
	if s.Tier != TierB {
		t.Fatalf("只命中「没讲取舍」应该是 B 档，实际: %s", s.Tier)
	}
	if r == nil || r.Comment != "答得偏浅" || len(r.WeakPoints) != 1 {
		t.Fatalf("评语不对: %+v", r)
	}
	if s.Partial || r.Partial {
		t.Fatal("原样读通的不该打 Partial 记号")
	}
}

// 只给了判卷结论、没给评语：不算失败。
func TestParseGrade_OnlyJudge(t *testing.T) {
	raw := JudgeOpen + `{"hits": []}` + JudgeClose

	s, r, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if s == nil {
		t.Fatal("判卷结论应该拿到")
	}
	if r != nil {
		t.Fatal("评语没给，应该是 nil")
	}
}

// 两块都没给：报错。
func TestParseGrade_Nothing(t *testing.T) {
	_, _, err := ParseGrade("我觉得这题答得还行")
	if !errors.Is(err, ErrNoGrade) {
		t.Fatalf("应该报 ErrNoGrade，实际: %v", err)
	}
}

// 命中项折算成档位。
//
// 这张表就是扣分制的全部规则，改一个数字都要在这儿体现出来。
func TestCombineTier(t *testing.T) {
	cases := []struct {
		name string
		hits []string
		want Tier
	}{
		{"一条都没踩", nil, TierA},
		{"只缺取舍", []string{"no_tradeoff"}, TierB},
		{"缺边界", []string{"no_boundary"}, TierB},
		{"缺取舍+没量化", []string{"no_tradeoff", "no_number"}, TierC},
		{"只报名词+没量化", []string{"name_only", "no_number"}, TierD}, // 2+1=3
		{"缺取舍+缺边界+没量化", []string{"no_tradeoff", "no_boundary", "no_number"}, TierD},
		{"答非所问", []string{"off_topic"}, TierD},                        // 3 格
		{"答非所问+没量化", []string{"off_topic", "no_number"}, TierE},      // 3+1=4
		{"核心概念说错", []string{"wrong_core"}, TierE},                    // 4 格，一步到 E
		{"回答过短", []string{"too_short"}, TierE},                        // 4 格
		{"重复写同一项只算一次", []string{"no_tradeoff", "no_tradeoff"}, TierB},
		{"不认识的项不影响档位", []string{"made_up"}, TierA},
	}

	for _, c := range cases {
		if got := CombineTier(c.hits); got != c.want {
			t.Errorf("%s：命中 %v 应该是 %s，实际 %s", c.name, c.hits, c.want, got)
		}
	}
}

// 模型报了一个不存在的扣分项：丢掉它，但要留记号。
//
// 丢掉是为了不让它把分数拉偏；留记号是为了让我们知道模型在胡说，
// 而不是悄悄当它没说过——悄悄修好是最危险的一种"修好"。
func TestScore_UnknownHit(t *testing.T) {
	raw := JudgeOpen + `{"hits": ["made_up", "no_tradeoff"]}` + JudgeClose

	s, _, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(s.Hits) != 1 || s.Hits[0] != "no_tradeoff" {
		t.Fatalf("不认识的项应该被丢掉，实际: %v", s.Hits)
	}
	if !s.Partial {
		t.Fatal("丢掉了模型报的项，必须打 Partial 记号")
	}
}

// 空列表不能留成 nil：JSON 化会变成 null，前端遍历就崩。
func TestScore_EmptyHits(t *testing.T) {
	raw := JudgeOpen + `{"hits": []}` + JudgeClose

	s, _, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if s.Hits == nil {
		t.Fatal("命中列表不能是 nil")
	}
}

// evidence 只保留还在 hits 里的键，免得显示时找不到对应条目。
func TestScore_EvidencePruned(t *testing.T) {
	raw := JudgeOpen + `{"hits": ["no_tradeoff"], "evidence": {"no_tradeoff": "只说了方案", "name_only": "悬空的依据"}}` + JudgeClose

	s, _, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if _, ok := s.Evidence["name_only"]; ok {
		t.Fatal("hits 里没有 name_only，它的依据应该被清掉")
	}
	if s.Evidence["no_tradeoff"] != "只说了方案" {
		t.Fatalf("在 hits 里的依据要留住，实际: %v", s.Evidence)
	}
}

// 代码预检：太短要判 too_short。
func TestCheckByCode_TooShort(t *testing.T) {
	hits := CheckByCode("不知道")
	if !hasHit(hits, "too_short") {
		t.Fatalf("不到 20 字应该判太短，实际: %v", hits)
	}
}

// 代码预检：够长但一个数字都没有，要判 no_number。
func TestCheckByCode_NoNumber(t *testing.T) {
	answer := "我把上下文分成三层，最近几轮保留原话，更早的压成摘要，跨会话的信息落数据库。"
	hits := CheckByCode(answer)
	if !hasHit(hits, "no_number") {
		t.Fatalf("通篇没有数字应该判没量化，实际: %v", hits)
	}
}

// 有数字的回答不该被判「没有量化」——这是防误杀的守门测试。
func TestCheckByCode_HasNumber(t *testing.T) {
	answer := "我把上下文分成 3 层，预算按 80K token 算，超了就把最早的几轮压成摘要。"
	hits := CheckByCode(answer)
	if hasHit(hits, "no_number") {
		t.Fatalf("有数字不该判没量化，实际: %v", hits)
	}
}

// 「几万」「30%」「两秒」这种也算给了量级，不该因为不是阿拉伯数字就扣分。
func TestCheckByCode_ScaleWords(t *testing.T) {
	for _, answer := range []string{
		"单次请求要发掉几万个 token，成本明显上去了，后来改成分层压缩。",
		"改完以后成本降了 30% 左右，延迟也下来了不少。",
		"超时我设的是两秒，超过就当作工具失败往下走。",
	} {
		if hits := CheckByCode(answer); hasHit(hits, "no_number") {
			t.Errorf("应该认出量级：%s → %v", answer, hits)
		}
	}
}

// 弱点列表给了 null：换成空列表，不能留空指针。
func TestReview_NullWeakPoints(t *testing.T) {
	raw := ReviewOpen + `{"comment": "一般", "weak_points": null}` + ReviewClose

	_, r, err := ParseGrade(raw)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if r.WeakPoints == nil {
		t.Fatal("弱点列表不能是 nil，前端会崩")
	}
}

// hasHit 判断一个命中列表里有没有某一项。
func hasHit(hits []string, want string) bool {
	for _, h := range hits {
		if h == want {
			return true
		}
	}
	return false
}
