package interview

import (
	"fmt"
	"strings"
	"time"
)

// FocusPoint 是本场要重点考的一个考点。
type FocusPoint struct {
	Tag        string
	Count      int    // 累计犯过几次
	LastDepth  int    // 上次讲到第几层，0 = 没记录
	LastDetail string // 上次具体差在哪
}

// PickFocus 挑出本场要重点追问的考点，最多 n 个。
//
// 挑谁，按这个顺序：
//  1. 复习排期里今天已经到期的 —— 本来就是你该练的
//  2. 没到期、但账本里次数最多的 —— 老毛病优先
//
// 为什么要排掉"已经好转"的：
// 连着几场都考一个已经会了的点，是浪费题，也会让你觉得这工具没用。
func PickFocus(s *Store, n int, now time.Time) ([]FocusPoint, error) {
	if n <= 0 {
		return nil, nil
	}

	trends, err := s.LoadTagTrends()
	if err != nil {
		return nil, err
	}
	due, err := s.DueReviews(now)
	if err != nil {
		return nil, err
	}
	weak, err := s.LoadWeakPoints()
	if err != nil {
		return nil, err
	}

	// 弱点账本里的"最近一次细节"和次数，比走势表更直接，直接拿来用。
	details := make(map[string]string, len(weak))
	counts := make(map[string]int, len(weak))
	for _, w := range weak {
		details[w.Tag] = w.LastDetail
		counts[w.Tag] = w.Count
	}

	seen := make(map[string]bool, len(weak)+len(due))
	out := make([]FocusPoint, 0, n)

	add := func(tag string) {
		// 三个提前退出：已经挑够、这个挑过了、这个已经好了。
		if len(out) >= n || seen[tag] {
			return
		}
		seen[tag] = true
		if settled(trends[tag]) {
			return
		}

		fp := FocusPoint{Tag: tag, Count: counts[tag], LastDetail: details[tag]}
		if h := trends[tag].Hits; len(h) > 0 {
			fp.LastDepth = h[len(h)-1].Depth
			if fp.Count == 0 {
				// 账本里没这个标签（比如只有 turn 有记录），就拿出现次数顶上。
				fp.Count = len(h)
			}
		}
		out = append(out, fp)
	}

	for _, d := range due {
		add(d.Tag)
	}
	// LoadWeakPoints 已经按次数降序排好，顺着加就是"次数多的优先"。
	for _, w := range weak {
		add(w.Tag)
	}
	return out, nil
}

// settled 判断这个毛病算不算"已经好转，不用再追"。
//
// 条件故意写得严：最近两次都得落在 A/B 档、而且整体是改善。
// 只看"改善"不够——从 E 挪到 D 也叫改善，那还得接着练。
func settled(t TagTrend) bool {
	if len(t.Hits) < 2 || t.Direction() != "改善" {
		return false
	}
	n := len(t.Hits)
	return t.Hits[n-1].Tier.Index() <= 1 && t.Hits[n-2].Tier.Index() <= 1
}

// FocusBrief 把重点拼成给面试官看的一段话。没有重点就返回空串。
//
// 只说"考什么、上次崩在哪一层"，不说"该怎么答"——
// 说了等于把参考答案递给考官，他会忍不住提示候选人。
func FocusBrief(points []FocusPoint) string {
	if len(points) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("本场重点（你自己心里有数就行，不要对候选人说出来）：\n")
	for _, p := range points {
		sb.WriteString("- ")
		sb.WriteString(p.Tag)
		switch {
		case p.LastDepth > 0 && p.Count > 0:
			fmt.Fprintf(&sb, "（上次讲到第 %d 层，累计 %d 次）", p.LastDepth, p.Count)
		case p.LastDepth > 0:
			fmt.Fprintf(&sb, "（上次讲到第 %d 层）", p.LastDepth)
		case p.Count > 0:
			fmt.Fprintf(&sb, "（累计 %d 次）", p.Count)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("在技术基础、项目深挖阶段找机会把它们问出来；" +
		"只答到「是什么」就继续往「为什么、边界」压。")
	return sb.String()
}

// FocusTags 返回考点名字，打印用。
func FocusTags(points []FocusPoint) string {
	names := make([]string, 0, len(points))
	for _, p := range points {
		names = append(names, p.Tag)
	}
	return strings.Join(names, "、")
}
