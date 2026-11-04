// Package trajectory 是"轨迹级评测"：不看单个答案答得好不好，
// 看一场面试的过程对不对。
//
// 它和打分器的区别，一句话：
//
//	打分器问「这句话质量如何」——需要模型、有波动、要标注集
//	本包问「这个行为合法吗」——纯规则、零成本、结果必然一致
//
// 所以它天生可复现：只读 turn 表，一次模型都不调。
// 同样的库跑一百遍，结论一模一样。
package trajectory

import (
	"fmt"
	"strings"
	"unicode"

	"tiny-agent/internal/interview"
)

// Level 是问题的严重程度。
//
// 为什么必须分级：如果所有问题都是"告警"，跑几场之后你就会开始无视
// 所有告警——这叫告警疲劳，是可观测性失效最常见的原因。
type Level string

const (
	LevelError Level = "error" // 违反了硬约束，属于 bug，必须修
	LevelWarn  Level = "warn"  // 不算 bug，但这场的质量可疑
)

// Finding 是一条命中的问题。
type Finding struct {
	Rule  string // 规则编号 T1~T8，统计违规率时按它聚合
	Level Level
	Msg   string
	Turns []int // 相关的题号，方便直接跳回那一题看原文
}

// Report 是一场面试的体检结果。
type Report struct {
	SessionID string
	Total     int
	Finished  bool
	Findings  []Finding
}

// Errors 返回硬违规条数。
func (r Report) Errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == LevelError {
			n++
		}
	}
	return n
}

// Warnings 返回软提示条数。
func (r Report) Warnings() int { return len(r.Findings) - r.Errors() }

// OK 判断这场有没有硬违规。
func (r Report) OK() bool { return r.Errors() == 0 }

// Check 是总入口：把 8 条规则全跑一遍。
//
// 规则全拆成独立函数，好处三个：
//  1. 每条能单独测（写单测只测 checkQuota，不用造一整场面试）
//  2. 以后想删/改一条，只动一个函数，不牵动别的
//  3. 加新规则就是往这里 append 一行
func Check(id string, turns []interview.Turn, finished bool) Report {
	r := Report{SessionID: id, Total: len(turns), Finished: finished}

	r.Findings = append(r.Findings, checkQuota(turns)...)
	r.Findings = append(r.Findings, checkCoverage(turns, finished)...)
	r.Findings = append(r.Findings, checkHardCap(turns)...)
	r.Findings = append(r.Findings, checkStageOrder(turns)...)
	r.Findings = append(r.Findings, checkDuplicate(turns)...)
	r.Findings = append(r.Findings, checkAnswer(turns)...)
	r.Findings = append(r.Findings, checkShortAnswer(turns)...)
	r.Findings = append(r.Findings, checkReverseQA(turns)...)

	return r
}

// T1：某个阶段问的题数，超过配额了吗。
//
// 这是状态机最核心的不变量。超了说明派生逻辑或推进逻辑有 bug，
// 你自己的体感就是"怎么还在问技术"。
func checkQuota(turns []interview.Turn) []Finding {
	count := make(map[interview.Stage]int)
	for _, t := range turns {
		count[t.Stage]++
	}

	var out []Finding
	for _, q := range interview.StageQuotas() {
		n := count[q.Stage]
		if n <= q.Quota {
			continue
		}
		// 把这个阶段超额的题号带上，方便直接跳过去看
		var idx []int
		for _, t := range turns {
			if t.Stage == q.Stage {
				idx = append(idx, t.Index)
			}
		}
		out = append(out, Finding{
			Rule:  "T1",
			Level: LevelError,
			Msg:   fmt.Sprintf("%s阶段问了 %d 题，配额只有 %d", q.Name, n, q.Quota),
			Turns: idx,
		})
	}
	return out
}

// T2：面完了，五个阶段都覆盖了吗。
//
// 关键：**只对"真正结束"的场次判**。中途退出只有 2 题是正常的，
// 对着它喊"你没面完"是误报。评测得知道自己在评什么。
func checkCoverage(turns []interview.Turn, finished bool) []Finding {
	if !finished {
		return nil
	}

	seen := make(map[interview.Stage]bool)
	for _, t := range turns {
		seen[t.Stage] = true
	}

	var missing []string
	for _, q := range interview.StageQuotas() {
		if !seen[q.Stage] {
			missing = append(missing, q.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Finding{{
		Rule:  "T2",
		Level: LevelError,
		Msg:   "标了结束但缺阶段：" + strings.Join(missing, "、"),
	}}
}

// T3：总题数超过硬上限了吗（MaxQuestions = 10）。
func checkHardCap(turns []interview.Turn) []Finding {
	if len(turns) <= interview.MaxQuestions {
		return nil
	}
	return []Finding{{
		Rule:  "T3",
		Level: LevelError,
		Msg:   fmt.Sprintf("一共问了 %d 题，硬上限是 %d", len(turns), interview.MaxQuestions),
	}}
}

// T4：阶段编号有没有往回退，或者出现不认识的值。
//
// 阶段是从 Turns 派生的，正常情况下只增不减。
// 一旦回退，说明派生函数被改坏了，或者库里有脏数据。
func checkStageOrder(turns []interview.Turn) []Finding {
	var out []Finding
	for i, t := range turns {
		if int(t.Stage) < 0 || int(t.Stage) >= len(interview.StageQuotas()) {
			out = append(out, Finding{
				Rule:  "T4",
				Level: LevelError,
				Msg:   fmt.Sprintf("第 %d 题的阶段值 %d 超出范围", t.Index, int(t.Stage)),
				Turns: []int{t.Index},
			})
			continue
		}
		if i > 0 && t.Stage < turns[i-1].Stage {
			out = append(out, Finding{
				Rule:  "T4",
				Level: LevelError,
				Msg: fmt.Sprintf("第 %d 题阶段回退：%s → %s",
					t.Index, turns[i-1].Stage.Name(), t.Stage.Name()),
				Turns: []int{turns[i-1].Index, t.Index},
			})
		}
	}
	return out
}

// T5：有没有重复问同一道题。
//
// 完全一样 → error（这场面试被浪费了）
// 前 20 字一样 → warn（可能是同一题换了个问法，先提示）
func checkDuplicate(turns []interview.Turn) []Finding {
	var out []Finding

	for i := 0; i < len(turns); i++ {
		for j := i + 1; j < len(turns); j++ {
			a := normalize(turns[i].Question)
			b := normalize(turns[j].Question)
			if a == "" || b == "" {
				continue
			}

			if a == b {
				out = append(out, Finding{
					Rule:  "T5",
					Level: LevelError,
					Msg:   fmt.Sprintf("第 %d 题和第 %d 题完全重复", turns[i].Index, turns[j].Index),
					Turns: []int{turns[i].Index, turns[j].Index},
				})
				continue
			}

			// 短的要是长的前缀，且长的够长，才算"疑似重复"
			short, long := a, b
			if len([]rune(short)) > len([]rune(long)) {
				short, long = long, short
			}
			if len([]rune(short)) >= 20 && strings.HasPrefix(long, short) {
				out = append(out, Finding{
					Rule:  "T5",
					Level: LevelWarn,
					Msg: fmt.Sprintf("第 %d 题和第 %d 题开头 20 字相同，疑似重复",
						turns[i].Index, turns[j].Index),
					Turns: []int{turns[i].Index, turns[j].Index},
				})
			}
		}
	}
	return out
}

// normalize 只留字母和数字，去掉标点和空格，再转小写。
//
// 为什么要归一化：模型问两遍同一道题，标点和空格常常不一样，
// 直接字符串比较会漏掉。
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// T6：有没有"问了但没答"的洞。
//
// 分两种情况，严重程度完全不同：
//   - 最后一题没答 → 正常（可能是中断），warn
//   - 中间某题没答却有下一题 → 数据坏了，error
func checkAnswer(turns []interview.Turn) []Finding {
	var out []Finding
	for i, t := range turns {
		if strings.TrimSpace(t.Answer) != "" {
			continue
		}
		if i == len(turns)-1 {
			out = append(out, Finding{
				Rule:  "T6",
				Level: LevelWarn,
				Msg:   fmt.Sprintf("第 %d 题还没有答案（可能是中途退出）", t.Index),
				Turns: []int{t.Index},
			})
			continue
		}
		out = append(out, Finding{
			Rule:  "T6",
			Level: LevelError,
			Msg:   fmt.Sprintf("第 %d 题没有答案，但后面还有题", t.Index),
			Turns: []int{t.Index},
		})
	}
	return out
}

// T7：回答是不是过短（少于 6 个字）。
//
// 反问环节排除：那一阶段是你在提问，不该用"答得好不好"的标准衡量。
func checkShortAnswer(turns []interview.Turn) []Finding {
	var short []int
	for _, t := range turns {
		if t.Stage == interview.StageReverseQA {
			continue
		}
		if len([]rune(strings.TrimSpace(t.Answer))) < 6 {
			short = append(short, t.Index)
		}
	}
	if len(short) == 0 {
		return nil
	}
	return []Finding{{
		Rule:  "T7",
		Level: LevelWarn,
		Msg:   fmt.Sprintf("%d 题回答过短（少于 6 字）", len(short)),
		Turns: short,
	}}
}

// T8：反问环节有没有被打分。
//
// 这条是专门盯着老 bug 的守门规则：反问是候选人在提问，
// 拿"答得好不好"评它是错的。现在 grade 应该是 skipped 或 pending，
// 一旦变成 done 就说明又犯回去了。
func checkReverseQA(turns []interview.Turn) []Finding {
	var out []Finding
	for _, t := range turns {
		if t.Stage != interview.StageReverseQA {
			continue
		}
		if t.Grade == interview.GradeDone {
			out = append(out, Finding{
				Rule:  "T8",
				Level: LevelError,
				Msg:   fmt.Sprintf("第 %d 题是反问环节，却被打了分", t.Index),
				Turns: []int{t.Index},
			})
		}
	}
	return out
}
