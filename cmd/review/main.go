// review 是「今天该练什么」的命令行工具。
//
// 它只做两件事：把到期的弱点列出来，把你的练习结果记回去。
// 不调模型、不联网，跑一次几毫秒。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"tiny-agent/internal/interview"
)

func main() {
	do := flag.Bool("do", false, "逐个录入练习结果")
	all := flag.Bool("all", false, "看全部标签的走势（不只是今天到期的）")
	backfill := flag.Bool("backfill", false, "把历史弱点补进复习排期（只需跑一次）")
	flag.Parse()

	store, err := interview.OpenStore("interview.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer store.Close()

	now := time.Now()

	if *backfill {
		n, err := store.BackfillReviews(now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "回填失败:", err)
			os.Exit(1)
		}
		fmt.Printf("回填了 %d 个标签，全部按今天到期处理。\n", n)
		return
	}

	trends, err := store.LoadTagTrends()
	if err != nil {
		fmt.Fprintln(os.Stderr, "读走势失败:", err)
		os.Exit(1)
	}

	if *all {
		printAll(trends)
		return
	}

	due, err := store.DueReviews(now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读排期失败:", err)
		os.Exit(1)
	}
	if len(due) == 0 {
		fmt.Printf("%s 今天没有要练的。\n", now.Format("2006-01-02"))
		fmt.Println("（如果刚做完记忆系统、库里有历史弱点，先跑一次：go run ./cmd/review -backfill）")
		return
	}

	fmt.Printf("%s 今天该练 %d 个\n\n", now.Format("2006-01-02"), len(due))
	for i, d := range due {
		fmt.Printf("%d. %s\n", i+1, d.Tag)
		fmt.Printf("   犯了 %d 次   走势 %s\n", d.Count, trends[d.Tag].Path())
		if d.LastDetail != "" {
			fmt.Printf("   上次：%s 第 %d 题 —— %s\n", d.LastSession, d.LastIdx, d.LastDetail)
		}
	}

	if !*do {
		fmt.Println("\n（练完加 -do 逐个录入结果）")
		return
	}

	// 逐个录结果。输入一行走一步，不要求你记住任何命令。
	fmt.Println()
	in := bufio.NewReader(os.Stdin)
	for i, d := range due {
		fmt.Printf("[%d/%d] %s\n  1=又忘了  2=还行  3=很熟  s=跳过 > ", i+1, len(due), d.Tag)
		line, _ := in.ReadString('\n')
		r, ok := parseResult(strings.TrimSpace(line))
		if !ok {
			continue // s 或别的什么，跳过
		}
		next, err := store.MarkReviewed(d.Tag, r, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  记失败：%v\n", err)
			continue
		}
		if next.IsZero() {
			continue
		}
		days := int(next.Sub(now).Hours()/24 + 0.5)
		fmt.Printf("  → 下次 %s（%d 天后）\n\n", next.Format("2006-01-02"), days)
	}
}

// parseResult 把一次按键翻译成三档结果。不认识的输入当作"跳过"。
// 直接回车也算"还行"——最省事的路径要留给最常见的答案。
func parseResult(s string) (interview.ReviewResult, bool) {
	switch strings.ToLower(s) {
	case "1", "a", "again":
		return interview.ResultAgain, true
	case "2", "g", "good", "":
		return interview.ResultGood, true
	case "3", "e", "easy":
		return interview.ResultEasy, true
	default:
		return "", false
	}
}

// printAll 打出全部标签的走势，按出现次数从多到少。
//
// 必须排序：map 遍历顺序是随机的，不排序你没法比两次输出的差异。
func printAll(trends map[string]interview.TagTrend) {
	if len(trends) == 0 {
		fmt.Println("还没有任何弱点记录：先跑一场面试。（走势靠 turn 表里的弱点算，不靠排期表）")
		return
	}

	tags := make([]string, 0, len(trends))
	for tag := range trends {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool {
		a, b := trends[tags[i]], trends[tags[j]]
		if len(a.Hits) != len(b.Hits) {
			return len(a.Hits) > len(b.Hits) // 次数多的在前
		}
		return tags[i] < tags[j] // 次数一样按名字排，保证稳定
	})

	for _, tag := range tags {
		t := trends[tag]
		fmt.Printf("%-28s %d 次  %s\n", tag, len(t.Hits), t.Path())
	}
}
