// 跑分器：拿标准集给打分器打一遍分，量出它到底准不准。
//
// 它是把秤，不是改秤。只做三件事：读标准集 → 调现有的打分器 → 算指标打表格。
//
// 用法：
//
//	go run ./cmd/evalcheck              跑一遍，看区分度
//	go run ./cmd/evalcheck -runs 3      连自洽率一起测（同一条打三次，看分数飘不飘）
//	go run ./cmd/evalcheck -review      只生成人工审核清单，不调模型、不花钱
//
// 关于人工审核：
//
//	标准集里的 expect_hits 是 AI 标的，不能当真理用。每条都带 reviewed 字段，
//	生成清单后逐条人工确认，认可的改成 true。-review 只写清单，不改数据。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tiny-agent/internal/interview"
)

// GoldenCase 是标准集里的一条。
//
// 注意：这里没有「期望分数」也没有「期望档位」字段。
// 档位一律由 expect_hits 算出来——人去判「这条有没有讲取舍」很准，
// 判「该给 6 分还是 7 分」很不准。这是这套评测集的核心规矩。
type GoldenCase struct {
	ID         string   `json:"id"`
	Question   string   `json:"question"`
	Answer     string   `json:"answer"`
	Quality    string   `json:"quality"` // 好 / 中上 / 中 / 中下 / 差 / 边界
	ExpectHits []string `json:"expect_hits"`
	Note       string   `json:"note"`
	Reviewed   bool     `json:"reviewed"` // 人工确认过没有
}

var qualityOrder = []string{"差", "中下", "中", "中上", "好"}

// runResult 是一条用例跑一次的结果。
type runResult struct {
	Case  GoldenCase
	Tier  interview.Tier
	Score int
	Hits  []string
	Err   string
}

func main() {
	var (
		file     = flag.String("file", filepath.Join("testdata", "golden", "cases.json"), "标准集路径")
		runs     = flag.Int("runs", 1, "每条跑几次（测自洽率用 3）")
		env      = flag.String("env", ".env", ".env 路径")
		quiet    = flag.Bool("q", false, "只打汇总，不打明细")
		review   = flag.Bool("review", false, "只生成人工审核清单，不调模型")
		reviewTo = flag.String("o", filepath.Join("testdata", "golden", "REVIEW.md"), "审核清单输出路径")
	)
	flag.Parse()

	cases, err := loadCases(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读标准集失败:", err)
		os.Exit(1)
	}

	// -review 模式：只写清单，不联网、不花钱
	if *review {
		if err := writeReview(*reviewTo, cases); err != nil {
			fmt.Fprintln(os.Stderr, "写审核清单失败:", err)
			os.Exit(1)
		}
		fmt.Printf("审核清单已生成：%s\n共 %d 条，已审核 %d 条，待审核 %d 条\n",
			*reviewTo, len(cases), countReviewed(cases), len(cases)-countReviewed(cases))
		return
	}

	loadEnv(*env)

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
		fmt.Fprintln(os.Stderr, "没找到 BAZ_OPENAI_API_KEY")
		os.Exit(1)
	}

	tags, err := interview.LoadTags("tags.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载标签库失败:", err)
		os.Exit(1)
	}

	chatter := interview.NewClaudeChatter(apiKey, baseURL, model)
	ctx := context.Background()

	fmt.Printf("=== golden set 跑分 ===\n")
	fmt.Printf("用例 %d 条 × 每条 %d 次 | 模型 %s\n", len(cases), *runs, model)

	// 标注没过人工审核，结论只能当参考——这句话要显眼，不然容易被当真理用
	if n := len(cases) - countReviewed(cases); n > 0 {
		fmt.Printf("⚠ 其中 %d 条标注还没人工审核（跑 `-review` 生成清单）\n", n)
	}
	fmt.Println()

	all := make(map[string][]runResult, len(cases))
	for _, c := range cases {
		for i := 0; i < *runs; i++ {
			r := runOnce(ctx, chatter, tags, c)
			all[c.ID] = append(all[c.ID], r)
			if !*quiet {
				printDetail(r)
			}
		}
	}

	if !*quiet {
		fmt.Println()
	}
	printSummary(cases, all, *runs)
}

// wantTier 是人工标注折算出来的期望档位。
func wantTier(c GoldenCase) interview.Tier {
	return interview.CombineTier(c.ExpectHits)
}

// runOnce 拿一条用例打一次分，失败会重试。
//
// 必须重试：模型偶尔返回空（中转抖动/限流），不重试的话量出来的
// 是「今天网络行不行」而不是「打分器准不准」。业务代码里的打分器
// 本来就有 3 次重试，秤也得有同样的待遇。
func runOnce(ctx context.Context, c interview.Chatter, tags *interview.TagBook, gc GoldenCase) runResult {
	var res runResult
	for attempt := 1; attempt <= 3; attempt++ {
		res = tryOnce(ctx, c, tags, gc)
		if res.Err == "" {
			return res
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return res
}

func tryOnce(ctx context.Context, c interview.Chatter, tags *interview.TagBook, gc GoldenCase) runResult {
	res := runResult{Case: gc}

	askCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	raw, err := c.Ask(askCtx, interview.GradeSystemPrompt(tags), []interview.Message{{
		Role: interview.RoleUser,
		Text: fmt.Sprintf("题目：%s\n\n候选人回答：%s", gc.Question, gc.Answer),
	}})
	if err != nil {
		res.Err = err.Error()
		return res
	}

	score, _, perr := interview.ParseGrade(raw)
	if score == nil {
		if perr != nil {
			res.Err = perr.Error()
		} else {
			res.Err = "没拿到判卷块"
		}
		return res
	}

	// 和业务代码走同一条路：代码预检先判一遍，再和模型报的合并。
	// 如果这里和业务代码判法不一样，量出来的就不是真实分数。
	score.Hits = interview.MergeHits(interview.CheckByCode(gc.Answer), score.Hits)
	score.Tier = interview.CombineTier(score.Hits)
	score.Total = score.Tier.Score()

	res.Score = score.Total
	res.Tier = score.Tier
	res.Hits = score.Hits
	return res
}

func printDetail(r runResult) {
	if r.Err != "" {
		fmt.Printf("   %-18s 出错：%s\n", r.Case.ID, r.Err)
		return
	}
	want := wantTier(r.Case)
	mark := "✓"
	if r.Tier != want {
		mark = "✗"
	}
	fmt.Printf("%s %-18s 人工=%-3s 期望=%-2s 实际=%-2s 分=%2d  命中=%v\n",
		mark, r.Case.ID, r.Case.Quality, want, r.Tier, r.Score, r.Hits)
}

func printSummary(cases []GoldenCase, all map[string][]runResult, runs int) {
	// 分组：五个质量档参与区分度计算，边界样本单独看
	byQuality := map[string][]int{}
	var edge []runResult

	for _, c := range cases {
		for _, r := range all[c.ID] {
			if r.Err != "" {
				continue
			}
			if c.Quality == "边界" {
				edge = append(edge, r)
				continue
			}
			byQuality[c.Quality] = append(byQuality[c.Quality], r.Score)
		}
	}

	avg := func(xs []int) float64 {
		if len(xs) == 0 {
			return 0
		}
		s := 0
		for _, x := range xs {
			s += x
		}
		return float64(s) / float64(len(xs))
	}

	fmt.Println("=== 汇总 ===")
	fmt.Println("\n【五档平均分】理想情况是严格递增（差最低，好最高）")
	for _, q := range qualityOrder {
		fmt.Printf("  %-3s  %4.1f 分  (%d 条)\n", q, avg(byQuality[q]), len(byQuality[q]))
	}

	// 单调性：分数应该随质量档一路往上（差 < 中下 < 中 < 中上 < 好）。
	//
	// 注意方向：qualityOrder 是从差排到好，所以后一档的分必须比前一档高。
	// 这里曾经写成 a >= prev 才算乱序，等于把「正常涨上去」当成错误——
	// 越准反而报的乱序越多，基线那句「2 处乱序」就是这么来的假数。
	//
	// 中间出现回退说明它分不开真实水平：比中下还差的回答，反而拿了更高的分。
	prev, breaks := -1.0, 0
	for _, q := range qualityOrder {
		if len(byQuality[q]) == 0 {
			continue
		}
		a := avg(byQuality[q])
		if prev >= 0 && a <= prev {
			breaks++
		}
		prev = a
	}
	mono := "✓ 严格递增"
	if breaks > 0 {
		mono = fmt.Sprintf("✗ 有 %d 处不递增（中间档分不开）", breaks)
	}
	fmt.Printf("  单调性：%s\n", mono)

	gap := avg(byQuality["好"]) - avg(byQuality["差"])
	verdict := "✗ 不合格"
	if gap >= 4 {
		verdict = "✓ 合格"
	}
	fmt.Printf("\n【区分度】好 − 差 = %.1f 分  %s（目标 ≥4）\n", gap, verdict)

	// 好档能不能到 A：这是扣分制改造的核心验收项。
	// 改造前 5 条全卡在 B——它不是不敢扣分，是永远不给满分。
	if good := byQuality["好"]; len(good) > 0 {
		top := 0
		for _, s := range good {
			if s >= 10 {
				top++
			}
		}
		v := "✗ 天花板还封着"
		if top*2 >= len(good) {
			v = "✓ 天花板开着"
		}
		fmt.Printf("【好档到 A】%d/%d 条拿到满分  %s\n", top, len(good), v)
	}

	// 档位命中：实际档位 vs 由人工标注算出的期望档位
	hit, total, offSum := 0, 0, 0
	for _, c := range cases {
		want := wantTier(c)
		for _, r := range all[c.ID] {
			if r.Err != "" {
				continue
			}
			total++
			d := r.Tier.Index() - want.Index()
			if d < 0 {
				d = -d
			}
			offSum += d
			if d == 0 {
				hit++
			}
		}
	}
	if total > 0 {
		fmt.Printf("\n【档位准确率】%d/%d 条完全对  %.0f%%\n", hit, total, float64(hit)/float64(total)*100)
		fmt.Printf("  平均偏差：%.1f 格\n", float64(offSum)/float64(total))
	}

	// 命中一致率：它扣的项和人工标的项重合多少。
	//
	// 为什么用 Jaccard 而不是「精确相等」：
	// 同一条回答可能踩 2 条也可能踩 3 条，只要大方向一致就算对齐。
	// 要求一字不差会把「双方都判对、只差一条」算成全错，指标就没意义了。
	var js []float64
	for _, c := range cases {
		for _, r := range all[c.ID] {
			if r.Err != "" {
				continue
			}
			js = append(js, jaccard(r.Hits, c.ExpectHits))
		}
	}
	if len(js) > 0 {
		s := 0.0
		for _, v := range js {
			s += v
		}
		rate := s / float64(len(js)) * 100
		v := "✗ 不合格"
		if rate >= 80 {
			v = "✓ 合格"
		}
		fmt.Printf("\n【命中一致率】%.0f%%  %s（目标 ≥80%%）\n", rate, v)
	}

	// 自洽率
	if runs > 1 {
		stable, t2 := 0, 0
		for _, c := range cases {
			var idxs []int
			for _, r := range all[c.ID] {
				if r.Err != "" {
					continue
				}
				idxs = append(idxs, r.Tier.Index())
			}
			if len(idxs) < 2 {
				continue
			}
			sort.Ints(idxs)
			t2++
			if idxs[len(idxs)-1]-idxs[0] <= 1 {
				stable++
			}
		}
		rate := 0.0
		if t2 > 0 {
			rate = float64(stable) / float64(t2) * 100
		}
		v := "✗ 不合格"
		if rate >= 90 {
			v = "✓ 合格"
		}
		fmt.Printf("\n【自洽率】%d/%d 条稳定  %.0f%%  %s（目标 ≥90%%）\n", stable, t2, rate, v)
	} else {
		fmt.Println("\n【自洽率】跳过（加 -runs 3 才测）")
	}

	// 边界样本：专治软肋，逐条列
	if len(edge) > 0 {
		fmt.Println("\n【边界样本】专测软肋，看它会不会被骗")
		for _, r := range edge {
			want := wantTier(r.Case)
			mark := "✓"
			if r.Tier != want {
				mark = "✗"
			}
			fmt.Printf("  %s %-14s 期望=%-2s 实际=%-2s 分=%2d  %s\n",
				mark, r.Case.ID, want, r.Tier, r.Score, r.Case.Note)
		}
	}

	if n := len(cases) - countReviewed(cases); n > 0 {
		fmt.Printf("\n⚠ 本次结论建立在 %d 条未经人工审核的标注上，只能当参考\n", n)
	}
}

// jaccard 算两个集合的重合度：交集 ÷ 并集。
//
// 两个都空 = 1.0（都说"一条没踩"就是完全一致），不要算成 0——
// 那会把「答得好的题双方都判没毛病」算成最差的一致率，指标就反了。
func jaccard(a, b []string) float64 {
	setA := make(map[string]bool, len(a))
	for _, x := range a {
		setA[x] = true
	}
	setB := make(map[string]bool, len(b))
	for _, x := range b {
		setB[x] = true
	}
	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}
	inter := 0
	for k := range setA {
		if setB[k] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 1.0
	}
	return float64(inter) / float64(union)
}

func countReviewed(cases []GoldenCase) int {
	n := 0
	for _, c := range cases {
		if c.Reviewed {
			n++
		}
	}
	return n
}

// writeReview 生成给人看的人工审核清单。
//
// 为什么单独生成一个 md 而不是让人去翻 JSON：
// 审核要做的事是「读回答、看标注、判断对不对」，JSON 里全是转义和引号，
// 一行两百个字，看着就想跳过。清单按条排好、把判断依据摊开，才审得下去。
func writeReview(path string, cases []GoldenCase) error {
	var b strings.Builder

	b.WriteString("# 评测集人工审核清单\n\n")
	b.WriteString("> 这份文件由 `go run ./cmd/evalcheck -review` 生成，改它没用，改 `cases.json` 才算数。\n\n")
	fmt.Fprintf(&b, "共 **%d** 条，已审核 **%d** 条，待审核 **%d** 条。\n\n",
		len(cases), countReviewed(cases), len(cases)-countReviewed(cases))

	b.WriteString("## 怎么审\n\n")
	b.WriteString("每条看两样：\n\n")
	b.WriteString("1. **回答本身**——它到底是哪个水平\n")
	b.WriteString("2. **我标的扣分项**——踩的这几条对不对、有没有漏\n\n")
	b.WriteString("认可就把 `cases.json` 里那一条的 `reviewed` 改成 `true`；不认可就改 `expect_hits`。\n\n")

	b.WriteString("## 扣分项速查\n\n")
	b.WriteString("| id | 含义 | 命中降几格 | 谁判 |\n|---|---|---|---|\n")
	for _, d := range interview.Deductions() {
		who := "模型"
		if d.Kind == interview.DeductByCode {
			who = "代码"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", d.ID, d.Name, d.Down, who)
	}
	b.WriteString("\n档位从 A 起算，命中就往下挪，挪到头是 E。A=10 B=8 C=6 D=4 E=2 分。\n\n")

	b.WriteString("---\n\n")

	for i, c := range cases {
		flag := "⬜ 待审"
		if c.Reviewed {
			flag = "✅ 已审"
		}
		want := wantTier(c)
		fmt.Fprintf(&b, "## %d. `%s`  %s\n\n", i+1, c.ID, flag)
		fmt.Fprintf(&b, "- **人工定的质量档**：%s\n", c.Quality)
		fmt.Fprintf(&b, "- **我标的扣分项**：%s\n", strings.Join(c.ExpectHits, "、"))
		fmt.Fprintf(&b, "- **折算档位**：%s（%d 分）\n", want, want.Score())
		fmt.Fprintf(&b, "- **我为什么这么标**：%s\n\n", c.Note)

		fmt.Fprintf(&b, "**题目**：%s\n\n", c.Question)
		fmt.Fprintf(&b, "**回答**：\n\n> %s\n\n", strings.ReplaceAll(c.Answer, "\n", "\n> "))
		fmt.Fprintf(&b, "**审核**：　[ ] 认可　　[ ] 要改（改成什么：________）\n\n")
		b.WriteString("---\n\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func loadCases(path string) ([]GoldenCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []GoldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s 里没有用例", path)
	}
	return cases, nil
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
