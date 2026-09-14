package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

//1.预算账本

//除了短期记忆的窗口上限，单次工具结果还有长期记忆返回也需要有一个token上限，

// available = Window − 输出预留 − 安全余量 − 工具结果预留
// history 预算 = available − tools − system − 记忆 − 本轮输入
type Budget struct {
	Window            int //模型上下文窗口
	MaxTokens         int //单次回复请求token上限
	MaxToolRounds     int //工具循环最大轮数
	SafetyMargin      int //估算开销，安全余量
	ToolResultReserve int //每轮工具结果token上限
	MemoryMaxTokens   int //长期记忆段上限
}

const (
	defaultContextWindow     = 131072 // 128k，真实值用 TINY_AGENT_CONTEXT_WINDOW 实测覆盖
	defaultMaxTokens         = 2048
	defaultMaxToolRounds     = 5
	defaultSafetyMargin      = 4096
	defaultToolResultReserve = 512
	defaultMemoryMaxTokens   = 1024
)

// 创造一个budget并且
func DefaultBudget() Budget {
	b := Budget{
		Window:            defaultContextWindow,
		MaxTokens:         defaultMaxTokens,
		MaxToolRounds:     defaultMaxToolRounds,
		SafetyMargin:      defaultSafetyMargin,
		ToolResultReserve: defaultToolResultReserve,
		MemoryMaxTokens:   defaultMemoryMaxTokens,
	}
	if v := os.Getenv("TINY_AGENT_CONTEXT_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			b.Window = n
		}
	}
	return b
}

func (b Budget) outputReserve() int { return b.MaxTokens * (b.MaxToolRounds + 1) }

func (b Budget) available() int {
	return b.Window - b.outputReserve() - b.SafetyMargin - b.ToolResultReserve*b.MaxToolRounds
}

//2.估算口径
// ---------- 2) 估算口径：全项目只能有一套 ----------

// estimateTextTokens 纯文本估算。
// 口径必须与 context.go 的 estimateTokens 完全一致（都是"字节数/3"）：
// 两套口径会让账本和实际发送内容对不上，超限时报错互相矛盾。
// 空串记 0：否则"没有记忆"也白占 1 token，账本会失真。
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/3 + 1
}

// estimateToolsTokens 工具说明书占用。
// 为什么必须算它：工具定义是每个请求都要发的固定开销。加一个工具的代价
// 往往不是"多一个函数"，而是"每一轮都多几百 token"——不记账就是隐形的。
func estimateToolsTokens(tools []anthropic.ToolUnionParam) int {
	total := 0
	for _, t := range tools {
		if data, err := json.Marshal(t); err == nil {
			total += len(data)/3 + 1
		}
	}
	return total
}

// historyTokensOf 一段消息的 token 总量。
// （context_test.go 里已有 totalTokens 这个测试侧 helper，同名会重复定义。）
// 注意：agent.go 工具循环里每轮全量重算是 O(n²)——有意的简单换正确，
// 当前 n 是几十条的量级无所谓；messages 上千条时应改为增量记账。
func historyTokensOf(h []anthropic.MessageParam) int {
	total := 0
	for _, m := range h {
		total += estimateTokens(m)
	}
	return total
}

// 3.装配报告，可观测性。
type SectionReport struct { //每个部分一张小票
	Name    string // tools / system(稳定) / system(记忆) / history / user(本轮)
	Budget  int    //预算多少token
	Used    int    //实际用了多少token
	Trimmed bool   //有没有因为超预算被裁过
}

// AssembleReport 一次装配的完整账本。
type AssembleReport struct { //总账单
	Window        int             // 模型窗口总量（全局：这次预算是从多大池子里分的）
	OutputReserve int             // 给模型输出预留了多少
	Available     int             //扣完硬开销后，能装内容的总量
	Sections      []SectionReport //每个槽位一个小票
	TotalUsed     int             //总用的token
	Warnings      []string        // 装配期异常（固定段超预算、history 前缀失效、工具循环撑爆等）
}

func (r *AssembleReport) addSection(name string, used, budget int, trimmed bool) {
	r.Sections = append(r.Sections, SectionReport{Name: name, Budget: budget, Used: used, Trimmed: trimmed})
}

func (r *AssembleReport) warn(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// String 账本快照，供 REPL 按需打印（TINY_AGENT_DEBUG_CONTEXT=1）。
func (r AssembleReport) String() string {
	var sb strings.Builder
	pct := 0
	if r.Available > 0 {
		pct = r.TotalUsed * 100 / r.Available
	}
	fmt.Fprintf(&sb, "[context] 窗口 %d = 输出预留 %d + 可用 %d | 实占 %d (%d%%)",
		r.Window, r.OutputReserve, r.Available, r.TotalUsed, pct)
	for _, s := range r.Sections {
		mark := ""
		if s.Trimmed {
			mark = " ✂"
		}
		fmt.Fprintf(&sb, "\n  %-14s %6d / %-6d%s", s.Name, s.Used, s.Budget, mark)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&sb, "\n  ! %s", w)
	}
	return sb.String()
}
