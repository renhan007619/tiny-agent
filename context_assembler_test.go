package main

import (
	"strings"
	"testing"

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

// checkLedger 不变式 ①②③
func checkLedger(t *testing.T, rep AssembleReport) {
	t.Helper()
	sum := 0
	for _, s := range rep.Sections {
		sum += s.Used
	}
	if sum != rep.TotalUsed {
		t.Errorf("账本不自洽：各段之和 %d != TotalUsed %d", sum, rep.TotalUsed)
	}
	for _, s := range rep.Sections {
		switch s.Name {
		case "tools", "system(稳定)", "user(本轮)": // 不可裁：给多少用多少
			if s.Used != s.Budget {
				t.Errorf("不可裁段 %s 的 Budget/Used 应相等：%d/%d", s.Name, s.Budget, s.Used)
			}
		default:
			// history 段的底线语义：裁到只剩最新一轮仍可能超预算，
			// 但超了必须在报告里留痕（超限必须可观测，不许静默）。
			if s.Used > s.Budget && len(rep.Warnings) == 0 {
				t.Errorf("段 %s 超出预算 %d > %d 且无告警", s.Name, s.Used, s.Budget)
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
		t.Errorf("实占 %d 超出可用预算 %d", asm.Report.TotalUsed, asm.Report.Available)
	}
	if got := firstText(asm.Messages[len(asm.Messages)-1]); got != "接着上面说" {
		t.Errorf("最后一条应是本轮输入，得到 %q", got)
	}
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
		t.Fatalf("system 应拆成 2 块，得到 %d 块", len(asm.System))
	}
	if asm.System[0].Text != "STABLE" {
		t.Errorf("第 0 块应是稳定指令，得到 %q", asm.System[0].Text)
	}
	if !strings.Contains(asm.System[1].Text, "变化的事实") {
		t.Errorf("变化内容应垫底，得到 %q", asm.System[1].Text)
	}
	if asm.System[0].CacheControl.Type == "" {
		t.Error("开启缓存时稳定块应带 cache_control 断点")
	}
	if asm.System[1].CacheControl.Type != "" {
		t.Error("变化块不该带断点（断点后的前缀每轮失效，打了白打）")
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
		t.Error("第一条记忆应保留")
	}
	if strings.Contains(text, "乙") {
		t.Error("第二条应整条丢弃（放不下丢整条，不许拼出半条）")
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
		t.Error("放不下第一条时应截断它（带省略号），而不是丢光")
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
		t.Error("极端预算下也不能丢本轮输入")
	}
	if len(asm.Report.Warnings) == 0 {
		t.Error("预算明显不够时应产生告警")
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
		t.Errorf("窗口小的反而保留更多消息：%d > %d", len(a.Messages), len(b.Messages))
	}
	checkLedger(t, a.Report)
	checkLedger(t, b.Report)
}
