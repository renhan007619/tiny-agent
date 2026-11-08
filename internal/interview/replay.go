package interview

import "errors"

// errEmpty 表示"原文读到了，但解析不出可用结果"。
//
// 它和"解析时抛了错"是两种失败，但对回放来说处理一样：这条跳过。
// 之所以要单独一个值，是为了让统计里"解析失败"和"没有基线"能分开说。
var errEmpty = errors.New("原始返回解析不出可用结果")

// Replayed 是一次回放的结果：同一条原始判词，用现在的代码重新算。
//
// 内嵌 RawGrade 是为了把"旧结论"直接摆在同一行方便对照：
// 写 r.OldTier / r.NewTier 一眼就能看出拿谁跟谁比。
type Replayed struct {
	RawGrade
	NewTier  Tier
	NewTotal int
	NewHits  []string
	Review   *Review
	Err      error
}

// ReplayOne 拿一条历史原始判词，用当前代码重新判一遍。
//
// 它和线上打分的**唯一区别是不调模型**：其余一模一样，
// 因为跑的是同一个 GradeFromRaw。这也是这套东西能当"验线上逻辑"用的原因。
func ReplayOne(r RawGrade) Replayed {
	out := Replayed{RawGrade: r}

	score, review, err := GradeFromRaw(r.Raw, r.Answer)
	if err != nil || score == nil {
		out.Err = err
		if err == nil {
			// 没报错但也没分出数来：原文里压根没有能用的判定块。
			out.Err = errEmpty
		}
		return out
	}

	out.NewTier = score.Tier
	out.NewTotal = score.Total
	out.NewHits = score.Hits
	out.Review = review
	return out
}

// Changed 判断这次回放的结论和当时不一样。
//
// 三件事都要成立才算"变了"：
//
//	没有解析失败   —— 算不出来不算变化，算失败
//	旧档位存在     —— 没有基线就无从比较，把它算成变化等于把"没存过"报成"改坏了"
//	新旧档位不同   —— 这才真叫变了
func (r Replayed) Changed() bool {
	return r.Err == nil && r.OldTier != "" && r.NewTier != r.OldTier
}
