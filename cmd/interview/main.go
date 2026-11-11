// 模拟面试官的命令行入口。
// 跑起来就是一场真面试：它问，你答，问够了就结束，然后出分。
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"tiny-agent/internal/interview"
)

func main() {
	loadEnv(".env")

	apiKey := os.Getenv("BAZ_OPENAI_API_KEY")
	baseURL := os.Getenv("BAZ_OPENAI_BASE_URL")
	model := os.Getenv("BAZ_OPENAI_MODEL")
	if baseURL == "" {
		baseURL = "https://api.lkeap.cloud.tencent.com/plan/anthropic"
	}
	if model == "" {
		model = "deepseek-v4-flash-202605"
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "没找到 BAZ_OPENAI_API_KEY：把 .env.example 复制成 .env，填上 key")
		os.Exit(1)
	}

	chatter := interview.NewClaudeChatter(apiKey, baseURL, model)

	// 标签库：读不到就直接退出，不要静默跑成"全都被归到未归类"。
	// 那种情况下报告照样能出，但弱点统计全是废的，比报错更难发现。
	tags, err := interview.LoadTags("tags.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载标签库失败:", err)
		fmt.Fprintln(os.Stderr, "（tags.txt 应该放在项目根目录，和 .env 同级）")
		os.Exit(1)
	}

	store, err := interview.OpenStore("interview.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer store.Close()

	// scanner 提到这里：问"要不要接着面"要用它读一行。
	// 原来它在下面 grader 之后才建，那时候已经来不及了。
	scanner := bufio.NewScanner(os.Stdin)

	var (
		session *interview.Session
		resumed bool
	)

	// 先把"有内容但没结束"的场次列出来，让人自己挑。
	//
	// 为什么不替你选：最后一条可能是一次误触（开了场立刻退），
	// 真正面到一半的是倒数第二条。候选通常就一两条，
	// 编号挑一次的成本远低于猜错的成本。
	open, err := store.FindOpenSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "（查未完成场次失败，按新的一场开：", err, "）")
	}

	switch len(open) {
	case 0:
		// 没有可续的，直接开新场，不打扰人。
	case 1:
		o := open[0]
		fmt.Printf("上次（%s 开始 · 已问 %d 题 · 已答 %d 题）没结束，接着面吗？[Y/n] ",
			o.StartedAt.Format("01-02 15:04"), o.Turns, o.Answered)
		if scanner.Scan() {
			line := strings.ToLower(strings.TrimSpace(scanner.Text()))
			if line != "n" { // 空回车也算续：绝大多数时候是想续的
				resumed = true
				session = resumeSession(store, o.ID, o.StartedAt)
			}
		}
	default:
		fmt.Println("发现几场没结束的面试：")
		for i, o := range open {
			fmt.Printf("  %d) %s 开始 · 已问 %d 题 · 已答 %d 题\n",
				i+1, o.StartedAt.Format("01-02 15:04"), o.Turns, o.Answered)
		}
		fmt.Print("续哪一场？（输序号，n 开新场）")
		if scanner.Scan() {
			var pick int
			if _, err := fmt.Sscan(strings.TrimSpace(scanner.Text()), &pick); err == nil &&
				pick >= 1 && pick <= len(open) {
				o := open[pick-1]
				resumed = true
				session = resumeSession(store, o.ID, o.StartedAt)
			}
		}
	}

	// 没接着面，就正常开一场新的。
	if session == nil {
		startedAt := time.Now()
		session = interview.NewSession(startedAt.Format("20060102-150405"), startedAt)
		if err := store.CreateSession(session.ID, startedAt); err != nil {
			fmt.Fprintln(os.Stderr, "建场次失败:", err)
			os.Exit(1)
		}
	}

	// 开场定一次本场重点：读历史弱点，挑两个这次要重点问的。
	//
	// 只挑一次，之后每轮 Prompt 都带同一段——内容稳定，不破坏缓存。
	// 库是空的（第一次用）就挑不出来，Prompt 自动走原来的分支，行为不变。
	focus, focusErr := interview.PickFocus(store, 2, time.Now())
	if focusErr != nil {
		fmt.Fprintf(os.Stderr, "（读历史弱点失败，本场按常规出题：%v）\n", focusErr)
	} else if len(focus) > 0 {
		session.Focus = focus
		fmt.Printf("本场重点 %d 个：%s\n", len(focus), interview.FocusTags(focus))
	}

	ctx := context.Background()

	// 打分工厂现在就开工：你答一题，它就接一题，后台慢慢算。
	grader := interview.NewGrader(chatter, store, tags, session.ID)

	// 总线：从这一刻起 main 不再直接喊打分器，只贴便签。
	//
	// 先订阅再 Run，顺序不能反——反过来的话，最早那几张便签
	// 可能在还没人听的时候就被消费掉了，而没人听的便签只会打一行日志就丢掉。
	//
	// 建在 grader.Start 之前：下面要给打分器装回调，回调里得用 bus。
	bus := interview.NewBus()
	bus.Subscribe(interview.NewEvaluator(grader, store))
	bus.Subscribe(interview.NewLogger()) // 第二个订阅者：加它只动了这一行
	go bus.Run(ctx)

	// 打分器打完一题，叫我们一声；我们把这事儿贴到总线上。
	//
	// 为什么用回调、而不是让打分器直接持有总线：
	// 打分器不需要知道"总线"这个东西存在，它只需要知道"打完了要说一声"。
	// 将来换成上报到别处、或者干脆不说，都不用改它。
	grader.OnGrade(func(t interview.Turn, sc *interview.Score, errText string) {
		if sc != nil {
			_ = bus.Publish(ctx, interview.Event{
				Type:      interview.EvGradeDone,
				SessionID: session.ID,
				Idx:       t.Index,
			})
			return
		}
		_ = bus.Publish(ctx, interview.Event{
			Type:      interview.EvGradeFailed,
			SessionID: session.ID,
			Idx:       t.Index,
			Err:       errors.New(errText),
		})
	})

	// Start 必须在装完回调之后：工人一开工就可能立刻打完一题，
	// 那时候回调还是 nil，这一题的通知就丢了。
	grader.Start(ctx)

	// 开考：告诉所有人这一场开始了。放在第一题之前，顺序对得上。
	if err := bus.Publish(ctx, interview.Event{
		Type:      interview.EvSessionStarted,
		SessionID: session.ID,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "（开考通知没发出去：", err, "）")
	}

	// 接着面的话，把上次欠下的打分补上。
	// 补的是"已经答过但没算分"的题，不是重新答题——所以不会漏东西。
	if resumed {
		owe, err := store.LoadUngradedTurns(session.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "（查欠打分失败：", err, "）")
		}
		for _, t := range owe {
			if err := bus.Publish(ctx, interview.Event{
				Type:      interview.EvQuestionAnswered,
				SessionID: session.ID,
				Idx:       t.Index,
			}); err != nil {
				fmt.Fprintln(os.Stderr, "（补打分的通知没发出去：", err, "）")
			}
		}
		if len(owe) > 0 {
			fmt.Printf("补打 %d 道题的分数（后台算，不耽误你继续答）\n", len(owe))
		}
		if session.Over() {
			fmt.Println("上次那场题已经问完了，补完分直接出报告。")
		}
	}

	fmt.Printf("模拟面试官就绪 | 模型=%s | 场次=%s | 标签 %d 个 | 全场最多 %d 题\n",
		model, session.ID, len(tags.All()), interview.MaxQuestions)
	fmt.Println("（打完一句话按回车提交；Ctrl+C 退出，下次启动可以接着面）")

	for !session.Over() {
		// 每轮 60 秒超时：模型那边卡住时，宁可报错也别一直干等。
		askCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		reply, err := chatter.Ask(askCtx, session.Prompt(), session.History())
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, ">>> 出错:", err)
			break
		}

		fmt.Println()
		fmt.Println("面试官：" + reply)
		session.AddQuestion(reply)

		last := session.Turns[len(session.Turns)-1]
		if err := store.SaveQuestion(session.ID, last); err != nil {
			fmt.Fprintln(os.Stderr, "（存题失败：", err, "）")
		}

		// 出题：和"答完了"是两件事，题号用刚存进去那道题的 Index。
		// 先落库再发，和答完那条一个规矩。
		if err := bus.Publish(ctx, interview.Event{
			Type:      interview.EvQuestionAsked,
			SessionID: session.ID,
			Idx:       last.Index,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "（出题通知没发出去：", err, "）")
		}

		fmt.Print("\n你：")
		if !scanner.Scan() {
			break // Ctrl+Z 或 EOF
		}
		answer := strings.TrimSpace(scanner.Text())
		session.Answer(answer)
		if err := store.SaveAnswer(session.ID, session.Total(), answer); err != nil {
			fmt.Fprintln(os.Stderr, "（存答失败：", err, "）")
		}

		// 答完贴一张便签，不等打分算完。
		//
		// 注意 SaveAnswer 必须在前：接收方是凭 sessionID + idx
		// 回库里读这一题的，先落库它才读得到。
		if err := bus.Publish(ctx, interview.Event{
			Type:      interview.EvQuestionAnswered,
			SessionID: session.ID,
			Idx:       session.Total(),
		}); err != nil {
			fmt.Fprintln(os.Stderr, "（答完这一题的通知没发出去：", err, "）")
		}
	}

	// 只有"题问够了"才算这场结束。
	//
	// 中途退出（模型出错、Ctrl+Z、Ctrl+C）绝不能标结束——那正是下次要续的场次。
	// 之前这里是无条件 EndSession，等于"一中断就宣告作废"，
	// 续接功能上线后这就是个致命 bug：面到一半 API 抖一下，整场就再也捡不回来了。
	if session.Over() {
		if err := store.EndSession(session.ID, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "（收尾失败：", err, "）")
		}

		// 结束：先落库再通知，和前面几处一个规矩。
		if err := bus.Publish(ctx, interview.Event{
			Type:      interview.EvSessionFinished,
			SessionID: session.ID,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "（收尾通知没发出去：", err, "）")
		}

		fmt.Println()
		fmt.Printf("面试结束（共 %d 题），等最后几题算完…\n", session.Total())
	} else {
		fmt.Println()
		fmt.Println("（这场没面完，已经存住了；下次启动可以接着面）")
		fmt.Printf("已完成 %d 题，等已经交上去的打分算完…\n", session.Total())
	}
	// 三段，顺序不能反。
	//
	// 1. Drain：等"答完一题"的便签都变成打分任务。
	//    少了这一步，还躺在 channel 里的便签永远没人处理，最后几题直接没分；
	//    更糟的是 Handle 正往已关闭的队列里写，那是 panic。
	// 2. Wait：等任务算完——这期间 worker 会冒出 grade.done / grade.failed。
	// 3. Drain：再排空一次，把这批"打分结果"也处理掉。
	//
	// 【必记】一次排空只能等一批事件；在"等"的过程中产生的事件是第二批，得再等一次。
	if err := bus.Drain(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "（等总线排空超时：", err, "）")
	}
	grader.Wait()
	if err := bus.Drain(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "（等打分结果通知发完超时：", err, "）")
	}

	// 报告从库里重新读一遍再出——你看到的，就是下次打开还能看到的。
	turns, err := store.LoadTurns(session.ID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "（读回记录失败，改用内存里的：", err, "）")
		turns = session.Turns
	}
	session.Turns = turns

	// 弱项累计也从库里读：报告展示的是"跨场次"的账，不是这一场的临时统计。
	weak, err := store.LoadWeakPoints()
	if err != nil {
		fmt.Fprintln(os.Stderr, "（读弱项累计失败：", err, "）")
	}

	fmt.Print(interview.Report(session, weak))
}

// resumeSession 把库里那场读回来。读不出来返回 nil，调用方会退回开新场。
//
// 注意这里只喂了 Turns 一个字段：阶段、题号、这个阶段问了几题，
// 全由它自己从 Turns 算出来。这是"状态不存、从事实派生"最直接的回报。
func resumeSession(store *interview.Store, id string, startedAt time.Time) *interview.Session {
	turns, err := store.LoadTurns(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "（读回上次记录失败，按新的一场开：", err, "）")
		return nil
	}
	if len(turns) == 0 {
		fmt.Fprintln(os.Stderr, "（上次那场没读到任何题目，按新的一场开）")
		return nil
	}
	s := interview.NewSession(id, startedAt)
	s.Turns = turns
	return s
}

// loadEnv 读 .env，一行一个 KEY=VALUE。
func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 没有 .env 就用系统环境变量
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
	}
}
