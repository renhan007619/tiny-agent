// trajectory 是轨迹评测的入口：给一场或全部面试做行为体检。
//
// 它和 cmd/evalcheck、cmd/ragcheck 是一家人：
//
//	evalcheck   评"打分器准不准"（要标注集、要调模型）
//	ragcheck    评"资料找得到吗"（要评测集、纯本地算）
//	trajectory  评"面试过程对不对"（不用标注、不调模型、只读库）
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"tiny-agent/internal/interview"
	"tiny-agent/internal/trajectory"
)

func main() {
	one := flag.String("session", "", "只体检这一场（场次 id）")
	all := flag.Bool("all", false, "体检全部场次，输出每条规则的命中率")
	flag.Parse()

	store, err := interview.OpenStore("interview.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer store.Close()

	switch {
	case *one != "":
		checkOne(store, *one)
	case *all:
		checkAll(store)
	default:
		fmt.Println("用法：")
		fmt.Println("  go run ./cmd/trajectory -session <场次id>")
		fmt.Println("  go run ./cmd/trajectory -all")
	}
}

// checkOne 体检一场。
func checkOne(store *interview.Store, id string) {
	turns, err := store.LoadTurns(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读记录失败:", err)
		os.Exit(1)
	}
	if len(turns) == 0 {
		fmt.Println("这场没有记录")
		return
	}

	// 是否结束必须单独问一次：LoadTurns 只给题目，不给场次状态。
	// 体检必须知道"这场面完了没有"，否则会对着中途退出的场面喊"阶段没覆盖"。
	finished := false
	if list, err := store.ListSessions(); err == nil {
		for _, b := range list {
			if b.ID == id {
				finished = b.Finished
				break
			}
		}
	}

	printReport(trajectory.Check(id, turns, finished))
}

// checkAll 体检全部场次，输出每条规则"多少比例的场次犯过"。
//
// 这就是 L3 的评测指标：不是分数，是违规率。
// 改完代码跑一遍，看它有没有降。
func checkAll(store *interview.Store) {
	list, err := store.ListSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "列场次失败:", err)
		os.Exit(1)
	}

	type ruleStat struct {
		sessions int // 命中过这条规则的场次数
		hits     int // 命中总次数
	}
	byRule := make(map[string]*ruleStat)

	var total, bad int
	for _, b := range list {
		turns, err := store.LoadTurns(b.ID)
		if err != nil || len(turns) == 0 {
			continue
		}
		total++

		r := trajectory.Check(b.ID, turns, b.Finished)
		if !r.OK() {
			bad++
		}

		// 同一场里同一条规则可能命中多次，按规则去重计场次
		seen := make(map[string]bool)
		for _, f := range r.Findings {
			st := byRule[f.Rule]
			if st == nil {
				st = &ruleStat{}
				byRule[f.Rule] = st
			}
			st.hits++
			if !seen[f.Rule] {
				st.sessions++
				seen[f.Rule] = true
			}
		}
	}

	if total == 0 {
		fmt.Println("库里还没有任何面试记录")
		return
	}

	fmt.Printf("\n场次 %d | 有硬违规 %d | 干净 %d\n", total, bad, total-bad)

	// 按规则编号排序输出，保证每次结果顺序一样（map 遍历是随机的）
	rules := make([]string, 0, len(byRule))
	for r := range byRule {
		rules = append(rules, r)
	}
	sort.Strings(rules)

	fmt.Printf("\n%-5s %10s %10s %8s\n", "规则", "命中场次", "命中率", "总次数")
	for _, r := range rules {
		st := byRule[r]
		fmt.Printf("%-5s %9d %9.0f%% %8d\n", r, st.sessions,
			float64(st.sessions)/float64(total)*100, st.hits)
	}

	fmt.Println("\n命中率 = 多少比例的场次犯过这条。改完代码再看它有没有降。")
}

// printReport 打一场的体检结果。
func printReport(r trajectory.Report) {
	state := "未完成"
	if r.Finished {
		state = "已结束"
	}
	fmt.Printf("\n场次 %s（%d 题，%s）\n", r.SessionID, r.Total, state)

	if len(r.Findings) == 0 {
		fmt.Println("全部通过 ✓")
		return
	}

	for _, f := range r.Findings {
		mark := "✗"
		if f.Level == trajectory.LevelWarn {
			mark = "⚠"
		}
		fmt.Printf("  %s [%s] %s", mark, f.Rule, f.Msg)
		if len(f.Turns) > 0 {
			fmt.Printf("（第 %v 题）", f.Turns)
		}
		fmt.Println()
	}
	fmt.Printf("\n合计：error %d 条，warn %d 条\n", r.Errors(), r.Warnings())
}
