package interview

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Grader 是一个打分小工厂：固定几个工人，从一个任务队列里领活。
//
// 为什么不是"每道题起一个协程"：
// 那样每来一道题就临时招一个工，人多了抢接口没人管。
// 固定工人 + 一个队列，同时干活的永远不超过 workers 个。
type Grader struct {
	chatter   Chatter
	store     *Store
	tags      *TagBook // 标签册子：把模型给的标签名对到库里
	sessionID string

	workers  int           // 固定开几个工人
	attempts int           // 每题最多试几次
	timeout  time.Duration // 单次打分最长等多久

	jobs    chan Turn      // 任务队列：交活的往里放，工人从里拿
	pending sync.WaitGroup // 记着还有几件活没干完
	closed  sync.Once
}

// NewGrader 建一个打分工厂。这时工人还没上班。
func NewGrader(c Chatter, s *Store, tags *TagBook, sessionID string) *Grader {
	return &Grader{
		chatter:   c,
		store:     s,
		tags:      tags,
		sessionID: sessionID,
		workers:   3,
		attempts:  3,
		timeout:   90 * time.Second,
		// 缓冲 32：一场最多 10 题，永远塞不满，交活的那一步不会卡住面试。
		jobs: make(chan Turn, 32),
	}
}

// Start 开局就叫工人上班，他们一直在这儿等活。
func (g *Grader) Start(ctx context.Context) {
	for i := 0; i < g.workers; i++ {
		go g.work(ctx)
	}
}

// work 是一个工人一辈子干的事：拿一件、干完、再拿下一件。
// 队列被关掉之后，这个 for 自己就退出了。
func (g *Grader) work(ctx context.Context) {
	for t := range g.jobs {
		g.gradeOne(ctx, t)
		g.pending.Done()
	}
}

// Submit 交一件活进去，立刻返回，谁也不用等。
func (g *Grader) Submit(t Turn) {
	// 反问环节是候选人在提问，不是他答题。
	// 拿「答得好不好」的标准去评「问得好不好」，本身就不成立——
	// 以前报告里出现「反问环节得 5 分」就是这么来的。
	if t.Stage == StageReverseQA {
		g.write(t.Index, GradeSkipped, nil, nil, "")
		return
	}
	if t.Answer == "" {
		// 没作答就没什么可打分的，给个明确结论，不要空着。
		g.write(t.Index, GradeUnavailable, nil, nil, "没有作答，不评分")
		return
	}
	g.pending.Add(1)
	g.jobs <- t
}

// Wait 关掉队列，等所有活干完。只在出报告前调一次。
func (g *Grader) Wait() {
	g.closed.Do(func() { close(g.jobs) }) // 关队列 = 告诉工人"没新活了，干完下班"
	g.pending.Wait()
}

// gradeOne 打一道题。重头戏是"失败怎么办"。
func (g *Grader) gradeOne(ctx context.Context, t Turn) {
	// 先写 running：这样中途看库能知道"它是正在算的时候挂的"，而不是根本没排上。
	g.write(t.Index, GradeRunning, nil, nil, "")

	var last string // 最后一次失败的原因，最后要写进报告

	for i := 1; i <= g.attempts; i++ {
		// 外面喊停（进程要退、整场超时），就别再试了。
		if ctx.Err() != nil {
			g.write(t.Index, GradeFailed, nil, nil, "调用方取消")
			return
		}
		// 第 2 次起先等一会儿再试，别对着抖动的服务连续砸。
		if i > 1 && !sleepCtx(ctx, backoff(i)) {
			g.write(t.Index, GradeFailed, nil, nil, "调用方取消")
			return
		}

		// 每次尝试单独给时限：一题卡住不能拖垮整批。
		// 提示词每次现拼：打分协议 + 当前标签清单。
		askCtx, cancel := context.WithTimeout(ctx, g.timeout)
		raw, err := g.chatter.Ask(askCtx, GradeSystemPrompt(g.tags), gradeMessages(t))
		cancel()

		if err != nil {
			last = err.Error()
			continue // 网络/模型抖动，值得再试
		}

		score, review, perr := ParseGrade(raw)
		if score == nil && review == nil {
			// 模型说话了，但没给能用的打分块。
			if perr != nil {
				last = perr.Error()
			} else {
				last = "打分结果为空"
			}
			continue
		}

		// 代码预检 + 合并。
		//
		// 为什么要多这一层：模型判的是「讲得对不对」，它很容易漏掉
		// 「通篇一个数字都没有」这种整体特征——它是在读内容，不是在数数字。
		// 而这件事代码百分百不会数错，还不花 token。
		//
		// 两边都判、合并去重，谁也不重复扣：
		// 代码报 no_number，模型也报 no_number，合并后还是一条。
		if score == nil {
			// 模型只给了评语没给判定：分数只有代码这一半，得标出来
			score = &Score{Partial: true}
		}
		score.Hits = MergeHits(CheckByCode(t.Answer), score.Hits)
		score.Tier = CombineTier(score.Hits)
		score.Total = score.Tier.Score()

		g.write(t.Index, GradeDone, score, review, "")

		// 分数落好之后，把这一题的弱点按标签记进跨场次的账本。
		// 注意顺序：先写分数，再记账——分数是这一场的事实，账本是长期的统计。
		if review != nil {
			g.recordWeakPoints(t.Index, review)
		}
		return
	}

	// 三次都没成：给它一个明确的死法，而不是留个空。
	g.write(t.Index, GradeUnavailable, nil, nil, last)
}

// recordWeakPoints 把这一题的弱点记进聚合表。
//
// 两个关键动作：
//  1. tag 先过一遍 Resolve —— 保证写进库的一定是标签库里存在的名字，
//     写歪的会退到父级、或者落进「未归类」，不会污染统计
//  2. 模型原本写的名字（rawTag）也一起存 —— 退到父级时细节不丢
func (g *Grader) recordWeakPoints(idx int, review *Review) {
	for _, w := range review.WeakPoints {
		tag := g.tags.Resolve(w.Tag)
		if err := g.store.UpsertWeakPoint(tag, w.Detail, w.Tag, g.sessionID, idx); err != nil {
			fmt.Fprintf(os.Stderr, "（第 %d 题弱点入库失败：%v）\n", idx, err)
		}
	}
}

// gradeMessages 把这一题的问答包成一次对话。
// 只给这一题：不给前面的题，免得模型被整体印象带软。
func gradeMessages(t Turn) []Message {
	return []Message{{
		Role: RoleUser,
		Text: fmt.Sprintf("题目：%s\n\n候选人回答：%s", t.Question, t.Answer),
	}}
}

// write 落库。写库失败只打日志，不中断其他题。
func (g *Grader) write(idx int, state GradeState, sc *Score, rv *Review, errText string) {
	if err := g.store.SaveGrade(g.sessionID, idx, state, sc, rv, errText); err != nil {
		fmt.Fprintf(os.Stderr, "（第 %d 题写库失败：%v）\n", idx, err)
	}
}

// backoff 第 attempt 次重试前等多久：2s、4s、8s……最多 30s。
func backoff(attempt int) time.Duration {
	d := 2 * time.Second * time.Duration(1<<uint(attempt-2))
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// sleepCtx 等一会儿，但外面喊停就立刻返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
