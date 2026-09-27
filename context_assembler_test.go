package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// ============ 上下文装配器测试 ============
//
// 与 context_test.go 同一套思路：装配器是纯函数，全部本地单测。
// 断言"账本必须自洽"这些不变式：
//   ① 各段 Used 之和 == TotalUsed（账不能对不上）
//   ② 不可裁段 Budget == Used；可裁段 Used <= Budget
//   ③ history 段允许超（TrimHistory 底线：宁超不切半轮），但超了必须有告警
//   ④ 正常窗口下 TotalUsed <= Available
//   ⑤ 排列不变式：稳定在前、易变垫底、断点只在稳定块
//   ⑥ 极端预算不 panic、不丢本轮输入
//
// 想让下面每条 ✓ 都显示出来，必须带 -v 跑：
//
//	go test -v .
//
// Go 默认只打印失败的测试，t.Logf 在不加 -v 且用例通过时会被丢弃——
// 这是 go test 的既定行为，不是测试文件的问题。

func testBudget(window int) Budget {
	b := DefaultBudget()
	b.Window = window
	return b
}

func testTools() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "get_color",
		Description: param.NewOpt("随机返回一个颜色"),
	}}}
}

// checkLedger 不变式 ①②③。每条都打 ✓/✗：带 -v 跑时输出是一份逐条对账的清单。
func checkLedger(t *testing.T, rep AssembleReport) {
	t.Helper()
	sum := 0
	for _, s := range rep.Sections {
		sum += s.Used
	}
	if sum != rep.TotalUsed {
		t.Errorf("✗ 不变式① 账本不自洽：各段之和 %d != TotalUsed %d", sum, rep.TotalUsed)
	} else {
		t.Logf("✓ 不变式① 账本自洽：Σ各段 Used = TotalUsed = %d", sum)
	}
	for _, s := range rep.Sections {
		switch s.Name {
		case "tools", "system(稳定)", "user(本轮)": // 不可裁：给多少用多少
			if s.Used != s.Budget {
				t.Errorf("✗ 不变式② 不可裁段 %s 的 Budget/Used 应相等：%d/%d", s.Name, s.Budget, s.Used)
			} else {
				t.Logf("✓ 不变式② %s 不可裁：Budget = Used = %d", s.Name, s.Used)
			}
		default:
			// history 段的底线语义：裁到只剩最新一轮仍可能超预算，
			// 但超了必须在报告里留痕（超限必须可观测，不许静默）。
			if s.Used > s.Budget && len(rep.Warnings) == 0 {
				t.Errorf("✗ 不变式③ 段 %s 超出预算 %d > %d 且无告警", s.Name, s.Used, s.Budget)
			} else if s.Used > s.Budget {
				t.Logf("✓ 不变式③ %s 超预算（%d > %d），但报告有 %d 条告警已留痕",
					s.Name, s.Used, s.Budget, len(rep.Warnings))
			} else {
				t.Logf("✓ 不变式③ %s 在预算内：%d ≤ %d", s.Name, s.Used, s.Budget)
			}
		}
	}
}

// 不变式 ①②④ + 本轮输入必须是最后一条消息
func TestAssembleLedgerSelfConsistent(t *testing.T) {
	asm := AssembleContext(AssembleInput{
		BaseSystem:  "You are Baz.",
		MemoryHits:  []string{"用户在做 tiny-agent 项目", "用户偏好 Go"},
		History:     buildTurns(), // 复用 context_test.go 的三轮历史
		UserInput:   "接着上面说",
		Tools:       testTools(),
		Budget:      testBudget(131072),
		EnableCache: true,
	})
	checkLedger(t, asm.Report)
	if asm.Report.TotalUsed > asm.Report.Available {
		t.Errorf("✗ 不变式④ 实占 %d 超出可用预算 %d", asm.Report.TotalUsed, asm.Report.Available)
	} else {
		t.Logf("✓ 不变式④ 实占 %d ≤ 可用 %d", asm.Report.TotalUsed, asm.Report.Available)
	}
	if got := firstText(asm.Messages[len(asm.Messages)-1]); got != "接着上面说" {
		t.Errorf("✗ 最后一条应是本轮输入，得到 %q", got)
	} else {
		t.Logf("✓ 最后一条是本轮输入：%q", got)
	}
	t.Logf("本次装配账本：\n%s", asm.Report.String())
}

// 不变式 ⑤：稳定在前、易变垫底，断点只打在稳定块上
func TestAssembleStablePrefixFirst(t *testing.T) {
	asm := AssembleContext(AssembleInput{
		BaseSystem:  "STABLE",
		MemoryHits:  []string{"变化的事实"},
		UserInput:   "hi",
		Budget:      testBudget(131072),
		EnableCache: true,
	})
	if len(asm.System) != 2 {
		t.Fatalf("✗ system 应拆成 2 块，得到 %d 块", len(asm.System))
	}
	t.Logf("✓ 不变式⑤ system 拆成 2 块（稳定块 + 记忆块）")
	if asm.System[0].Text != "STABLE" {
		t.Errorf("✗ 第 0 块应是稳定指令，得到 %q", asm.System[0].Text)
	} else {
		t.Logf("✓ 第 0 块 = 稳定指令：%q", asm.System[0].Text)
	}
	if !strings.Contains(asm.System[1].Text, "变化的事实") {
		t.Errorf("✗ 变化内容应垫底，得到 %q", asm.System[1].Text)
	} else {
		t.Logf("✓ 第 1 块 = 变化内容（记忆垫底）：%q", asm.System[1].Text)
	}
	if asm.System[0].CacheControl.Type == "" {
		t.Error("✗ 稳定块应带 cache_control 断点，实际 Type 是空串" +
			"（检查装配器是否用了 anthropic.NewCacheControlEphemeralParam() 而不是零值构造）")
	} else {
		t.Logf("✓ 稳定块带断点：Type = %q", asm.System[0].CacheControl.Type)
	}
	if asm.System[1].CacheControl.Type != "" {
		t.Errorf("✗ 变化块不该带断点（断点后的前缀每轮失效，打了白打），实际 Type = %q",
			asm.System[1].CacheControl.Type)
	} else {
		t.Logf("✓ 变化块无断点（Type 为空）")
	}
}

// 记忆控量：放不下就丢整条，绝不产生半条
func TestMemoryDropsWholeEntry(t *testing.T) {
	hits := []string{
		strings.Repeat("甲", 200), // 每条约 201 token
		strings.Repeat("乙", 200),
		strings.Repeat("丙", 200),
	}
	b := testBudget(131072)
	b.MemoryMaxTokens = 250 // head(6) + 第一条(201+1) = 208 ≤ 250 < 208 + 第二条

	asm := AssembleContext(AssembleInput{
		MemoryHits: hits,
		UserInput:  "hi",
		Budget:     b,
	})
	text := ""
	for _, blk := range asm.System {
		text += blk.Text
	}
	if !strings.Contains(text, "甲") {
		t.Error("✗ 第一条记忆应保留")
	} else {
		t.Logf("✓ 记忆控量：第一条保留（甲）")
	}
	if strings.Contains(text, "乙") {
		t.Error("✗ 第二条应整条丢弃（放不下丢整条，不许拼出半条）")
	} else {
		t.Logf("✓ 记忆控量：第二条整条丢弃，未拼出半条（无乙）")
	}
	checkLedger(t, asm.Report)
}

// 记忆控量：一条都放不下时截断第一条，而不是丢光
func TestMemoryTruncatesWhenNothingFits(t *testing.T) {
	b := testBudget(131072)
	b.MemoryMaxTokens = 100
	asm := AssembleContext(AssembleInput{
		MemoryHits: []string{strings.Repeat("长", 500)},
		UserInput:  "hi",
		Budget:     b,
	})
	if len(asm.System) == 0 || !strings.HasSuffix(asm.System[len(asm.System)-1].Text, "…") {
		t.Error("✗ 放不下第一条时应截断它（带省略号），而不是丢光")
	} else {
		t.Logf("✓ 一条都放不下：截断第一条（末尾带省略号），记忆未整段丢空")
	}
	checkLedger(t, asm.Report)
}

// 不变式 ⑥：极端预算不 panic、不丢本轮输入、必有告警
func TestAssembleTinyWindowNoPanic(t *testing.T) {
	asm := AssembleContext(AssembleInput{
		BaseSystem: strings.Repeat("x", 9000),
		MemoryHits: []string{strings.Repeat("记", 3000)},
		History:    buildTurns(),
		UserInput:  "还在吗",
		Tools:      testTools(),
		Budget:     testBudget(2000),
	})
	checkLedger(t, asm.Report)
	if len(asm.Messages) == 0 || firstText(asm.Messages[len(asm.Messages)-1]) != "还在吗" {
		t.Error("✗ 极端预算下也不能丢本轮输入")
	} else {
		t.Logf("✓ 极端预算下本轮输入仍在最后一条：%q", "还在吗")
	}
	if len(asm.Report.Warnings) == 0 {
		t.Error("✗ 预算明显不够时应产生告警")
	} else {
		t.Logf("✓ 预算不足产生 %d 条告警（未 panic）", len(asm.Report.Warnings))
	}
}

// history 吃剩余：窗口越大保留的历史越多
func TestHistoryEatsRemainder(t *testing.T) {
	base := AssembleInput{BaseSystem: "sys", History: buildTurns(), UserInput: "hi", Tools: testTools()}

	small := base
	small.Budget = testBudget(1800)
	large := base
	large.Budget = testBudget(131072)

	a, b := AssembleContext(small), AssembleContext(large)
	if len(a.Messages) > len(b.Messages) {
		t.Errorf("✗ 窗口小的反而保留更多消息：%d > %d", len(a.Messages), len(b.Messages))
	} else {
		t.Logf("✓ history 吃剩余：小窗口保留 %d 条 ≤ 大窗口保留 %d 条", len(a.Messages), len(b.Messages))
	}
	checkLedger(t, a.Report)
	checkLedger(t, b.Report)
}

// ============ L0 尺寸闸门（truncateToolResult / toolResultQuota）测试 ============
//
// 为什么这么测：
//  1. L0 的承诺不是"结果等于某个固定字符串"，而是"截断之后的估算 token
//     绝不超过配额"。这是账本与闸门同源的前提——只要破一次，报告里的数字
//     就和实际发出去的内容对不上，而 API 报 400 时你会先怀疑 API、
//     不会怀疑自己的闸门。
//  2. 所以主体是"配额区间扫描"的不变式，定点断言只钉住边界行为
//     （未超不截、恰好等于不截、配额非正全丢）。
//  3. 配额一律现算、不写死：写死会随标记文案改动而假红。
//  4. 唯一破约的地方（配额连标记本身都装不下）单独测并写进注释：
//     它是已知契约边界，不是 bug，但必须钉住——否则将来会有人"顺手修好"，
//     把留痕能力一起改掉。

// sampleLongCN 足够长的中文样本：含多字节字符，用来验证"不切出半个字符"。
// 500 × 15 字节 ≈ 7500 字节 ≈ 2501 token，远大于任何正常配额。
func sampleLongCN() string { return strings.Repeat("中文内容abc", 500) }

// markerTokens 截断标记自身的估算开销。
// 标记里要写"原始约 N token、单条上限 M token"，位数随内容变化，
// 所以按真实数字现算，而不是拍一个常量。
func markerTokens(used, quota int) int {
	return estimateTextTokens(fmt.Sprintf(toolResultMarkerFmt, used, quota))
}

// TestTruncateToolResult 定点钉住三种分支 + 两个退化输入。
func TestTruncateToolResult(t *testing.T) {
	const short = "hello world"       // 4 token
	exact := strings.Repeat("a", 297) // 297/3+1 = 100 token，恰好等于配额
	over := strings.Repeat("a", 300)  // 101 token，只超 1
	cases := []struct {
		name    string
		text    string
		quota   int
		wantCut bool
	}{
		{"远小于配额·原样返回", short, 100, false},
		{"恰好等于配额·不截断（判据是 <= 不是 <）", exact, 100, false},
		{"只超 1 token·截断", over, 100, true},
		{"中文超长·截断且留痕", sampleLongCN(), 512, true},
		{"配额 0·全丢但仍返回 cut=true 让调用方留痕", short, 0, true},
		{"负配额·不 panic", short, -5, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, cut := truncateToolResult(c.text, c.quota)
			if cut != c.wantCut {
				t.Fatalf("cut=%v, want %v（quota=%d, used=%d）",
					cut, c.wantCut, c.quota, estimateTextTokens(c.text))
			}
			if !cut {
				if got != c.text {
					t.Fatal("未截断时必须原样返回（零改动）")
				}
				return
			}
			if !utf8.ValidString(got) {
				t.Fatal("截断结果不是合法 UTF-8：把中文切成了半个字符")
			}
			if c.quota <= 0 {
				// 配额非正 = 这条结果没有生存空间：内容全丢，只靠 cut=true
				// 让 agent.go 打 warning 留痕（标记塞不进去，也没有内容可截）。
				if got != "" {
					t.Fatalf("配额非正应返回空内容，实际 %q", got)
				}
				return
			}
			if !strings.Contains(got, "已截断") {
				t.Fatalf("静默截断：结果里没有留痕标记，got=%q", got)
			}
		})
	}
}

// TestTruncateToolResultQuotaInvariant 核心不变式：配额能装下标记时，
// 截断结果绝不超配额。
//
// 起点为什么不写死：标记开销随"原始 token 数"的位数变化，而配额从 1 开始扫时，
// 前几个配额连标记自己都放不下——那是另一条契约
// （见 TestTruncateToolResultTinyQuotaKnownLimit）。这里动态判定"装得下"再断言，
// 文案一改测试不会假红，契约一改又会真红。
func TestTruncateToolResultQuotaInvariant(t *testing.T) {
	text := sampleLongCN()
	used := estimateTextTokens(text)

	checked, skipped, maxSkipped := 0, 0, 0
	for q := 1; q <= 512; q++ {
		if markerTokens(used, q) > q {
			skipped++
			maxSkipped = q
			continue
		}
		checked++
		out, cut := truncateToolResult(text, q)
		if !cut {
			t.Fatalf("配额 %d 下应当截断（原文 %d token）", q, used)
		}
		if got := estimateTextTokens(out); got > q {
			t.Fatalf("配额 %d：截断后仍占 %d token —— 账本与闸门不同源", q, got)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("配额 %d：截出了半个 UTF-8 字符", q)
		}
		if !strings.Contains(out, "已截断") {
			t.Fatalf("配额 %d：静默截断，没留痕", q)
		}
	}

	// 反向自检：若没有任何配额被跳过，说明这条测试根本没扫到区间，
	// 或者契约边界已被改掉——两种情况都该有人来看一眼。
	if skipped == 0 {
		t.Fatal("没有任何配额被跳过：本测试的前提（存在装不下标记的极小配额）已不成立，请复核")
	}
	if maxSkipped > 64 {
		t.Fatalf("配额 %d 仍装不下标记：标记开销异常膨胀了（现在约 %d token）",
			maxSkipped, markerTokens(used, 1))
	}
	t.Logf("✓ 配额 1..512 扫描：守住「截断后不超配额」%d 个，按契约边界跳过 %d 个（最大跳过配额 %d）",
		checked, skipped, maxSkipped)
}

// TestTruncateToolResultTinyQuotaKnownLimit 已知契约边界。
//
// 配额小于标记自身开销（约 20 token）时，"截断后不超配额"不成立：
// 返回的就只剩标记，而标记本身就超了配额。
//
// 这是有意的取舍，不是漏修：留痕（告诉模型"这条不完整、原始约 N token"）
// 优先于 1~20 token 的账目精确——返回空内容又不留痕，模型只会更困惑，
// 排查的人也无从知道这里发生过截断。
//
// 现实中会走到这里的路径：toolResultQuota 的防御下限 q=1
// （一轮出现极多工具调用、预留被均分到 1 token 时）。
func TestTruncateToolResultTinyQuotaKnownLimit(t *testing.T) {
	used := estimateTextTokens(sampleLongCN())
	out, cut := truncateToolResult(sampleLongCN(), 1)
	if !cut {
		t.Fatal("配额 1 下应当标记为已截断")
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("小配额下也必须留痕，got=%q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatal("小配额下也必须是合法 UTF-8")
	}
	if estimateTextTokens(out) <= 1 {
		t.Fatal("行为已变化：小配额下不再超配额。若改成了极简标记，请同步更新 " +
			"context_assembler.go 的契约注释与 TestTruncateToolResultQuotaInvariant 的起算逻辑")
	}
	t.Logf("✓ 边界已记录：配额 1 下结果仍占 %d token，标记开销 %d token、内容 %q",
		estimateTextTokens(out), markerTokens(used, 1),
		fmt.Sprintf(toolResultMarkerFmt, used, 1))
}

// TestToolResultQuota 配额均分与两条防御规则。
func TestToolResultQuota(t *testing.T) {
	cases := []struct {
		name       string
		reserve, n int
		want       int
	}{
		{"单次调用拿全部预留", 512, 1, 512},
		{"两个调用均分", 512, 2, 256},
		{"五个调用均分（默认 MaxToolRounds 的量级）", 512, 5, 102},
		{"零调用视作单次（防御）", 512, 0, 512},
		{"负调用数视作单次（防御）", 512, -1, 512},
		{"调用数远超预留·保底 1 而不是 0", 512, 10000, 1},
		{"预留只有 1·保底仍为 1", 1, 1000, 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := toolResultQuota(c.reserve, c.n); got != c.want {
				t.Fatalf("toolResultQuota(%d, %d) = %d, want %d", c.reserve, c.n, got, c.want)
			}
		})
	}
}

// TestTruncateByTokens 字节/3 口径下的反向截断：合法 UTF-8 是硬要求。
func TestTruncateByTokens(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		tokens int
		want   string
	}{
		{"非正配额返回空", "abc", 0, ""},
		{"负配额返回空", "abc", -1, ""},
		{"短于配额原样返回", "abc", 10, "abc"},
		{"恰好等于配额·原样返回", "abcdefghi", 3, "abcdefghi"},
		{"超长·截断后追加省略号", "abcdefghijkl", 2, "abcdef…"},
		{"切点落在多字节字符中间·回退到字符起点", "a中中", 1, "a…"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := truncateByTokens(c.in, c.tokens)
			if got != c.want {
				t.Fatalf("truncateByTokens(%q, %d) = %q, want %q", c.in, c.tokens, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("结果不是合法 UTF-8：%q", got)
			}
		})
	}
}
