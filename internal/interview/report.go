package interview

import (
	"fmt"
	"sort"
	"strings"
)

// Report 把一场面试拼成一份给人看的文字。
//
// 第二个参数 weak 是"跨场次"的弱项累计（从 weak_point 表读出来的），
// 不是这一场的临时统计——因为"这个毛病我到底犯过几次"才是真正有用的信息。
func Report(s *Session, weak []WeakStat) string {
	var b strings.Builder

	fmt.Fprintf(&b, "\n========== 面试报告 ==========\n")
	fmt.Fprintf(&b, "场次：%s\n题目：%d 道\n\n", s.ID, len(s.Turns))

	var scored, unscored, sum int

	for _, t := range s.Turns {
		fmt.Fprintf(&b, "第 %d 题 [%s]\n", t.Index, t.Stage.Name())
		fmt.Fprintf(&b, "  问：%s\n", oneLine(t.Question))
		if t.Answer != "" {
			fmt.Fprintf(&b, "  答：%s\n", oneLine(t.Answer))
		}

		switch t.Grade {
		case GradeDone:
			if t.Score != nil {
				scored++
				sum += t.Score.Total
				// 以前这里是一行「深度 7 · 项目 5 · 沟通 8 · 解题 6」。
				// 现在改成档位 + 踩了哪几条坑——因为「7 分」没法照着改，
				// 「没讲取舍」可以照着改。报告是给人复盘用的，不是给人比大小的。
				fmt.Fprintf(&b, "  分：%s 档（%d/10）%s\n",
					t.Score.Tier, t.Score.Total, partialMark(t.Score.Partial))

				// L2：讲到第几层。它和档位是两回事——
				// 零命中能拿 A 档，但也可能只讲到第 2 层（边界没提）。
				fmt.Fprintf(&b, "  深：%s\n", depthText(t.Score.Depth))
				if t.Score.Suspect != "" {
					fmt.Fprintf(&b, "  疑：%s\n", oneLine(t.Score.Suspect))
				}

				if len(t.Score.Hits) == 0 {
					fmt.Fprintln(&b, "  扣分：无（这条回答没踩坑）")
				} else {
					fmt.Fprintf(&b, "  扣分 %d 条：\n", len(t.Score.Hits))
					for _, id := range t.Score.Hits {
						line := "    · " + DeductionName(id)
						if ev := t.Score.Evidence[id]; ev != "" {
							line += "　——" + oneLine(ev)
						}
						fmt.Fprintln(&b, line)
					}
				}
			}
			if t.Review != nil {
				if t.Review.Comment != "" {
					fmt.Fprintf(&b, "  评：%s\n", t.Review.Comment)
				}
				for _, w := range t.Review.WeakPoints {
					tag := w.Tag
					if tag == "" {
						tag = "未标注"
					}
					fmt.Fprintf(&b, "  · [%s] %s\n", tag, w.Detail)
				}
			}
		case GradeSkipped:
			// 反问环节是候选人在提问，本来就不存在"答得好不好"。
			// 它既不该出分，也不该占「未评分」的名额——那是留给"没打成"的。
			fmt.Fprintln(&b, "  分：不计分（反问环节，是你在提问）")
		default:
			unscored++
			fmt.Fprintf(&b, "  分：未评分（%s）\n", gradeNote(t))
		}
		fmt.Fprintln(&b)
	}

	if len(weak) > 0 {
		fmt.Fprintln(&b, "—— 弱项累计（跨场次，按模块分组）——")
		for _, g := range groupWeak(weak) {
			fmt.Fprintf(&b, "%s（共 %d 次）\n", g.Name, g.Total)
			for _, w := range g.Items {
				short := strings.TrimPrefix(w.Tag, g.Name+"/")
				mark := ""
				if w.LastSession == s.ID {
					mark = "  ← 本场"
				}
				fmt.Fprintf(&b, "  %d 次  %s%s\n", w.Count, short, mark)
				if w.LastDetail != "" {
					fmt.Fprintf(&b, "       最近：%s\n", oneLine(w.LastDetail))
				}
			}
		}
		fmt.Fprintln(&b)
	}

	if scored > 0 {
		fmt.Fprintf(&b, "平均分：%.1f（%d 题有效）\n", float64(sum)/float64(scored), scored)
	}
	if unscored > 0 {
		fmt.Fprintf(&b, "未评分：%d 题——不代表 0 分，可以重跑\n", unscored)
	}
	return b.String()
}

// WeakGroup 是报告里的一组：一个模块下面挂着几个具体毛病。
type WeakGroup struct {
	Name  string
	Total int // 这一组的总次数——是加出来的，不是存出来的
	Items []WeakStat
}

// groupWeak 把扁平的弱点按"斜杠前面那一段"分组。
//
// 注意：模块的总次数是现场加出来的，不是从表里读的。
// 表里只存最细的那一格，父级数字不落库——同一个事实只存一份，
// 不然改一处漏一处，两边永远对不上。
func groupWeak(weak []WeakStat) []WeakGroup {
	index := map[string]*WeakGroup{}
	var order []string

	for _, w := range weak {
		name := TagUnclassified
		if i := strings.Index(w.Tag, "/"); i > 0 {
			name = w.Tag[:i]
		}

		g, ok := index[name]
		if !ok {
			g = &WeakGroup{Name: name}
			index[name] = g
			order = append(order, name)
		}
		g.Total += w.Count
		g.Items = append(g.Items, w)
	}

	out := make([]WeakGroup, 0, len(order))
	for _, name := range order {
		out = append(out, *index[name])
	}
	// 错得最多的模块排前面；一样多就保持原来的顺序（sort 是不稳定的，所以用 SliceStable）
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}

func partialMark(p bool) string {
	if p {
		return "〔含修补值〕"
	}
	return ""
}

// depthText 把层数翻成人话。
//
// 0 单独一支：它不是"第 0 层"，而是"这题没判出层次"——
// 老数据、或者模型没填，都会是 0。把它说成"第 0 层"会让人以为有个负分档。
func depthText(d int) string {
	switch d {
	case 1:
		return "第 1 层——只说了是什么"
	case 2:
		return "第 2 层——讲了为什么 / 怎么做"
	case 3:
		return "第 3 层——连边界和取舍也讲了"
	default:
		return "未判定"
	}
}

func gradeNote(t Turn) string {
	switch t.Grade {
	case GradePending:
		return "还没轮到"
	case GradeRunning:
		return "还在算"
	case GradeSkipped:
		return "不计分（反问环节）"
	case GradeFailed:
		return "被打断，可重跑"
	case GradeUnavailable:
		if t.GradeErr != "" {
			return "重试用尽：" + oneLine(t.GradeErr)
		}
		return "重试用尽"
	default:
		return "未知状态"
	}
}

// oneLine 把多行压成一行；太长的截掉。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
