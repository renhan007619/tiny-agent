// replay 是"改完打分逻辑，先离线看影响面"的工具。
//
// 它不调模型：读库里存的历史原始判词，用当前代码重新算一遍。
// 所以跑多少次结果都一样，几十毫秒出结果。
//
// 它能回答的只有一个问题：「我这次改，动了多少条？」
// 「改完是不是更好了」它答不了——那要有真值，得走 cmd/evalcheck 那条路。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"tiny-agent/internal/interview"
)

func main() {
	one := flag.String("session", "", "只回放这一场")
	verbose := flag.Bool("v", false, "逐条列出档位变了的题")
	flag.Parse()

	store, err := interview.OpenStore("interview.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer store.Close()

	rows, err := store.LoadRawGrades(*one)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读原始判词失败:", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Println("可回放 0 条：库里还没有存过原始判词。")
		fmt.Println("（grade_raw 是这次才加的，只有从现在开始的场次才有）")
		return
	}

	// 三个数各管一件事，不能混：
	//
	//	parsed   原文能解析的条数
	//	compared 新旧档位真的能比较的条数（有旧基线）——档位变化率的分母
	//	baseN    当时真的存过分的条数           —— 平均分的分母
	//
	// 为什么分母不能共用 parsed：没有基线的条目既不该进平均分，
	// 也不该进档位变化统计，塞进去等于拿"没数据"的题稀释结论。
	var (
		parsed   int
		compared int
		changed  int
		baseN    int
		sumOld   int
		sumNew   int
	)
	trans := make(map[string]int) // 档位流转："B→C" -> 几条

	for _, r := range rows {
		res := interview.ReplayOne(r)
		if res.Err != nil {
			continue // 这条原文本来就用不了，跳过
		}
		parsed++

		// 平均分只统计"当时真的存过分"的条目。
		// total 是 NULL 的会被读成 0，直接加进去会把基线压低、涨幅抬高。
		if r.OldTotalValid {
			sumOld += r.OldTotal
			sumNew += res.NewTotal
			baseN++
		}

		// 没旧档位就没得比：既不算变化，也不进流转表。
		if r.OldTier == "" {
			continue
		}
		compared++
		if res.NewTier != r.OldTier {
			changed++
			trans[string(r.OldTier)+"→"+string(res.NewTier)]++

			if *verbose {
				fmt.Printf("\n%s 第 %d 题  %s → %s（%d → %d 分）\n",
					r.SessionID, r.Index, r.OldTier, res.NewTier, r.OldTotal, res.NewTotal)
				fmt.Printf("  旧 hits：%v\n  新 hits：%v\n", r.OldHits, res.NewHits)
				fmt.Printf("  模型原话：%s\n", clip(r.Raw, 120))
			}
		}
	}

	fmt.Printf("\n可回放 %d 条（带原始判词），解析成功 %d 条\n", len(rows), parsed)

	if compared > 0 {
		fmt.Printf("档位可比 %d 条，变化 %d 条（%.0f%%）——这是变化率，不是优化率\n",
			compared, changed, float64(changed)/float64(compared)*100)
	} else {
		fmt.Println("档位可比 0 条：这批数据没有可用的旧档位基线")
	}

	if baseN > 0 {
		avgOld := float64(sumOld) / float64(baseN)
		avgNew := float64(sumNew) / float64(baseN)
		fmt.Printf("平均分（%d 条有基线）%.2f → %.2f（%+.2f）\n",
			baseN, avgOld, avgNew, avgNew-avgOld)
		// 相对变化必须防除零：旧均分为 0 时百分比没有意义。
		if sumOld > 0 {
			fmt.Printf("相对变化 %+.1f%%\n", float64(sumNew-sumOld)/float64(sumOld)*100)
		}
	} else {
		fmt.Println("平均分无法比较：这批数据没有存过旧分数")
	}

	if len(trans) > 0 {
		fmt.Println("\n档位流转：")
		keys := make([]string, 0, len(trans))
		for k := range trans {
			keys = append(keys, k)
		}
		sort.Strings(keys) // map 遍历随机，排序保证输出稳定
		for _, k := range keys {
			fmt.Printf("  %-6s %d 条\n", k, trans[k])
		}
	}
}

// clip 把长文本压成一行，预览用。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
