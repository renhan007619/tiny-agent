package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

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
	MessageBudget int             // messages 段配额（含本轮输入）= Available − tools − system − 记忆
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

//3) 装配：记账按优先级，排列按稳定性

type AssembleInput struct {
	BaseSystem  string                     // 稳定系统指令（MinCore.systemPrompt）
	MemoryHits  []string                   // 长期记忆命中，按相关度降序
	History     []anthropic.MessageParam   // 历史对话（不含本轮输入）
	UserInput   string                     // 本轮用户输入
	Tools       []anthropic.ToolUnionParam // 工具说明书（口径 = 实际发出去的那份）
	Budget      Budget
	EnableCache bool // 是否在稳定块打 cache_control 断点（默认关，理由见下）
}

// Assembled 装配结果：直接喂给 Messages API 的三样东西 + 一份账。
type Assembled struct {
	System   []anthropic.TextBlockParam
	Messages []anthropic.MessageParam
	Tools    []anthropic.ToolUnionParam
	Report   AssembleReport
}

const memoryHead = "相关记忆: \n"

// AssembleContext 装配一次请求的上下文。纯函数：同样输入必得同样输出。
//
// 记账顺序 = 优先级（固定段 > 记忆段 > history 吃剩余）；
// 排列顺序 = 稳定性（稳定在前、易变垫底）。两条轴独立。
func AssembleContext(in AssembleInput) Assembled {
	b := in.Budget
	rep := AssembleReport{Window: b.Window, OutputReserve: b.outputReserve(), Available: b.available()}
	if rep.Available <= 0 {
		rep.warn("窗口 %d 覆盖不了输出预留 %d + 余量，预算配置有误", b.Window, rep.OutputReserve)
	}

	// --- 记账 1：固定段（不可裁，只能如实上报） ---
	toolsUsed := estimateToolsTokens(in.Tools)
	sysUsed := estimateTextTokens(in.BaseSystem)
	rep.addSection("tools", toolsUsed, toolsUsed, false)
	rep.addSection("system(稳定)", sysUsed, sysUsed, false)

	// --- 记账 2：记忆段（可裁：先丢整条，再截断） ---
	memText, memUsed, memTrimmed := fitMemory(in.MemoryHits, b.MemoryMaxTokens)
	rep.addSection("system(记忆)", memUsed, b.MemoryMaxTokens, memTrimmed)
	if len(in.MemoryHits) > 0 && memText == "" {
		rep.warn("命中 %d 条记忆但一条都放不下（MemoryMaxTokens=%d 过小）", len(in.MemoryHits), b.MemoryMaxTokens)
	}

	// --- 记账 3：messages 吃剩余（本轮输入先保证，history 吃剩下） ---
	msgBudget := rep.Available - toolsUsed - sysUsed - memUsed
	if msgBudget < 0 {
		rep.warn("固定段(tools+system+记忆)已用 %d，超出可用预算 %d，history 只能保留最新一轮",
			toolsUsed+sysUsed+memUsed, rep.Available)
		msgBudget = 0
	}
	// 必须在 clamp 之后入账：记 clamp 前的负数会让 agent.go 工具循环那道
	// "messages 有没有涨破配额"的护栏失真，护栏等于没有。
	rep.MessageBudget = msgBudget
	userUsed := estimateTextTokens(in.UserInput)
	histBudget := msgBudget - userUsed
	if histBudget < 0 {
		histBudget = 0
	}
	history := TrimHistory(in.History, histBudget)
	histUsed := historyTokensOf(history)
	rep.addSection("history", histUsed, histBudget, len(history) < len(in.History))
	rep.addSection("user(本轮)", userUsed, userUsed, false)

	if histBudget > 0 && histUsed > histBudget {
		rep.warn("history 裁到只剩最新一轮仍占 %d > 预算 %d（TrimHistory 底线：宁超不切半轮）",
			histUsed, histBudget)
	}
	if len(history) < len(in.History) {
		// 裁剪位置 = 前缀失效位置。缓存匹配的是"字节流的开头"：从头部删一轮，
		// 后面所有轮次整体前移、位置全变，messages 段缓存需重建。这条告警
		// 是"缓存省了多少"的唯一线索，也是将来做滞后裁剪（超到 110% 才裁回
		// 90%，用可控超额换裁剪频率下降）的观测依据。
		rep.warn("history 从头部裁掉 %d 条消息 -> messages 前缀变化，prompt cache 需重建",
			len(in.History)-len(history))
	}

	// --- 排列：稳定在前，易变垫底 ---
	var system []anthropic.TextBlockParam
	if in.BaseSystem != "" {
		blk := anthropic.TextBlockParam{Text: in.BaseSystem}
		if in.EnableCache {
			// 断点打在"最后一个稳定 block"上：Anthropic 的缓存是
			// tools -> system -> messages 的连续前缀，断点表示"到此（含 tools）可缓存"。
			// 为什么默认关（两个现实）：① 官方要求前缀达到最小长度（1024/2048 token）
			// 才真写入缓存，我们这点 system 远不够；② 记忆块每轮变化，杀伤半径
			// 不止自己——它之后整个 messages 的缓存都陪葬。真正的修复（记忆挪
			// messages 尾部 / 会话内冻结记忆）属于路线图"缓存友好布局"。
			// 这里只保证结构正确：实测条件满足后可随时打开。
			blk.CacheControl = anthropic.CacheControlEphemeralParam{}
		}
		system = append(system, blk)
	}
	if memText != "" {
		// 每轮变化的记忆垫底：旧版拼进同一个 block，等于让稳定部分陪着失效。
		system = append(system, anthropic.TextBlockParam{Text: memText})
	}

	// 不复用 history 的底层数组：append 会写进共享数组的容量区，与调用方
	// 持有的旧切片形成别名，这类 bug 调试时极难发现。
	messages := make([]anthropic.MessageParam, 0, len(history)+1)
	messages = append(messages, history...)
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(in.UserInput)))

	rep.TotalUsed = toolsUsed + sysUsed + memUsed + histUsed + userUsed
	return Assembled{System: system, Messages: messages, Tools: in.Tools, Report: rep}
}

// fitMemory 把记忆命中控量后拼成一段文本。
//
// 策略（为什么这么做）：每条命中是一整条独立事实，直接按字节砍尾巴会把最后
// 一条砍成残句，模型很可能把残句当完整事实（"用户在腾讯云工作"→"用户在腾讯"）。
// 所以：按相关度降序收，放不下就丢整条并停止（后面的更不相关，一并丢）；
// 一条都放不下时，才截断第一条——宁要残句，也不要把记忆整段丢空。
func fitMemory(hits []string, maxTokens int) (text string, used int, trimmed bool) {
	headUsed := estimateTextTokens(memoryHead)
	if len(hits) == 0 || maxTokens <= headUsed {
		return "", 0, false
	}
	used = headUsed
	kept := make([]string, 0, len(hits))
	for _, h := range hits {
		if h == "" {
			continue
		}
		cost := estimateTextTokens(h) + 1 // +1 = join 时的换行
		if used+cost > maxTokens {
			trimmed = true
			if len(kept) == 0 { // 连第一条都放不下 -> 截断它
				tr := truncateByTokens(h, maxTokens-used)
				kept = append(kept, tr)
				used = headUsed + estimateTextTokens(tr) + 1
				if used > maxTokens {
					used = maxTokens // 省略号"…"3字节可能让估算回弹，保守记账
				}
			}
			break
		}
		kept = append(kept, h)
		used += cost
	}
	if len(kept) == 0 {
		return "", 0, trimmed
	}
	return memoryHead + strings.Join(kept, "\n"), used, trimmed
}

// truncateByTokens 按估算口径反向截断。
// 口径是 len(s)/3，所以 n token ≈ 3n 字节；从边界往前退到合法 UTF-8 字符起点，
// 免得把中文切出半个字符（无效字节会让 JSON 编码和模型输入都出问题）。
func truncateByTokens(s string, tokens int) string {
	if tokens <= 0 {
		return ""
	}
	maxBytes := tokens * 3
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
