// 模拟面试官的命令行入口。
// 跑起来就是一场真面试：它问，你答，问够了就结束，然后出分。
package main

import (
	"bufio"
	"context"
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

	startedAt := time.Now()
	session := interview.NewSession(startedAt.Format("20060102-150405"), startedAt)
	if err := store.CreateSession(session.ID, startedAt); err != nil {
		fmt.Fprintln(os.Stderr, "建场次失败:", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// 打分工厂现在就开工：你答一题，它就接一题，后台慢慢算。
	grader := interview.NewGrader(chatter, store, tags, session.ID)
	grader.Start(ctx)

	scanner := bufio.NewScanner(os.Stdin)

	fmt.Printf("模拟面试官就绪 | 模型=%s | 场次=%s | 标签 %d 个 | 全场最多 %d 题\n",
		model, session.ID, len(tags.All()), interview.MaxQuestions)
	fmt.Println("（打完一句话按回车提交，Ctrl+C 退出）")

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

		fmt.Print("\n你：")
		if !scanner.Scan() {
			break // Ctrl+Z 或 EOF
		}
		answer := strings.TrimSpace(scanner.Text())
		session.Answer(answer)
		if err := store.SaveAnswer(session.ID, session.Total(), answer); err != nil {
			fmt.Fprintln(os.Stderr, "（存答失败：", err, "）")
		}

		// 答完立刻交给打分工厂，不等它算完。
		grader.Submit(session.Turns[len(session.Turns)-1])
	}

	if err := store.EndSession(session.ID, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "（收尾失败：", err, "）")
	}

	fmt.Println()
	fmt.Printf("面试结束（共 %d 题），等最后几题算完…\n", session.Total())
	grader.Wait()

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
