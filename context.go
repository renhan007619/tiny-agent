package main

import (
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
)

// ============ 滑动窗口 · 上下文管理（短期记忆的"闸门"） ============
//
// 问题：history 每轮只增不减，长对话 token 一路涨到模型窗口上限
//       -> API 报 context length exceeded / 质量劣化。
// 解法：每次调用模型前，把 history 压回预算内——超预算就从最旧的
//       一轮开始整轮丢弃（滑动窗口：窗口永远停在时间轴最右端）。
//
// 两个设计约束（为什么这么做）：
//  1. 量纲是 token 而不是"条数"：消息长短悬殊（一次工具结果可能几百
//     token），模型限制的也是 token 总量。预算不能拍脑袋，按公式算：
//       预算 = 窗口 − system − 本轮输入 − maxTokens×(maxRounds+1) − 工具结果 − 安全余量
//     输出侧有硬上界所以可算：单次回复 ≤ maxTokens（2048），
//     工具循环 ≤ maxRounds（5）次，故输出预留 = 2048×6。
//  2. 丢弃按"完整轮"而不是"单条消息"：assistant 的 tool_use 和它后面
//     user 的 tool_result 靠 id 配对，拆散任一半 API 都会 400。
//     每轮 = 一条"新一轮提问(user 纯文本)"到下一个提问之前的所有消息。

// estimateTokens 粗略估算一条消息的 token 数。
// 不引第三方 tokenizer（anthropic SDK 不带）：按消息 JSON 体积/3 估算——
// 中文 UTF-8 每字 3 字节 ≈ 1 token，英文会略高估。高估无害、低估才危险，
// 预算本来就留了安全余量，足以吸收估算误差。
func estimateTokens(m anthropic.MessageParam) int {
	data, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	return len(data)/3 + 1
}

// blockType 取一个 content block 的 JSON type 字段（text / tool_use / tool_result）。
// 用 JSON 探测而不是类型断言，避开 SDK 各 block 具体结构的差异，稳。
func blockType(b anthropic.ContentBlockParamUnion) string {
	var probe struct {
		Type string `json:"type"`
	}
	if data, err := json.Marshal(b); err == nil {
		_ = json.Unmarshal(data, &probe)
	}
	return probe.Type
}

// isNewUserTurn 判断这条 user 消息是不是"新一轮提问"。
// user 分两种：普通提问（content 是 text）和 tool_result 回填
// （content 是 tool_result，role 被迫写 user——见 agent.go 的 3/3 注释）。
// 只有前者才是一轮对话的起点，后者必须跟它的 assistant 同生共死。
func isNewUserTurn(m anthropic.MessageParam) bool {
	if m.Role != "user" {
		return false
	}
	for _, b := range m.Content {
		if blockType(b) == "tool_result" {
			return false
		}
	}
	return true
}

// TrimHistory 滑动窗口裁剪：把 history 压回预算内。
// 思路（正向）：超预算就从最旧的一轮开始整轮丢，丢完还超就再丢下一轮；
// 直到 total ≤ 预算，或只剩最新一轮（底线，永不丢，宁超不切半轮）。
func TrimHistory(history []anthropic.MessageParam, budgetTokens int) []anthropic.MessageParam {
	if len(history) == 0 {
		return history
	}

	// 每条消息只估算一次，顺手得总量；没超预算就原样返回（切片零拷贝）
	toks := make([]int, len(history))
	total := 0
	for i, m := range history {
		toks[i] = estimateTokens(m)
		total += toks[i]
	}
	if total <= budgetTokens {
		return history
	}

	// 切轮：0 号兜底为第一轮起点，之后每逢"新一轮提问"开启新轮
	starts := []int{0}
	for i := 1; i < len(history); i++ {
		if isNewUserTurn(history[i]) {
			starts = append(starts, i)
		}
	}

	// ★ 核心：从最旧一轮开始丢，丢到不超预算为止；最多丢到只剩最新一轮。
	// first = 当前"保留范围"最旧一端的起点下标；每丢一轮就 +1 往后挪。
	// 条件 first < len(starts)-1 同时保证 starts[first+1] 不越界。
	first := 0
	for first < len(starts)-1 && total > budgetTokens {
		// 丢掉第 first 轮：区间 [starts[first], starts[first+1]) 的所有消息
		for i := starts[first]; i < starts[first+1]; i++ {
			total -= toks[i] // 账本同步扣掉被丢的量
		}
		first++ // 保留范围的起点往后挪一轮
	}
	return history[starts[first]:]
}
