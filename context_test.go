package main

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// ============ 滑动窗口（TrimHistory）测试 ============
//
// 为什么这么测：
//  1. TrimHistory 是纯函数（历史 + 预算 -> 历史），不碰网络、不碰 DB、不要 key，
//     所以全部用本地单测覆盖，go test 毫秒级跑完。
//  2. 它的正确性不是"结果等于某个固定切片"，而是"结果必须满足几条结构不变式"：
//     ① 结果非空（底线：宁超不切半轮）
//     ② 首条必须是"新一轮提问"（不许切在轮中间）
//     ③ tool_use / tool_result 按 id 成对（拆散任一半，API 直接 400）
//     ④ 要么回到预算内，要么已退到只剩最新一轮
//     所以主体是 checkInvariants，定点断言只用来钉住几个具体行为。
//  3. 预算一律用 estimateTokens 现算，不写死常量：
//     写死会随文案/消息结构改动而假红，而且会跟被估算的实现耦合。
//     下面所有定点断言用的都是"精确算出来的预算"，不依赖估算的具体数值。

// ---------- 构造器：把 SDK block union 的噪音挡在测试体外 ----------

func userText(s string) anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewTextBlock(s))
}

func asstText(s string) anthropic.MessageParam {
	return anthropic.NewAssistantMessage(anthropic.NewTextBlock(s))
}

// asstToolUse 构造带 tool_use 的 assistant 消息。
// input 用 json.RawMessage，与 agent.go 回填历史的写法保持一致。
func asstToolUse(id, name string) anthropic.MessageParam {
	return anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock(id, json.RawMessage(`{"city":"shenzhen"}`), name))
}

// userToolResult 构造 tool_result 回填消息。
// 注意：它的 role 是 user，但 content 是 tool_result —— 不是"新一轮提问"。
func userToolResult(id string) anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewToolResultBlock(id, "22°C", false))
}

// ---------- 测试数据 ----------

// buildTurns 造 3 轮历史，轮边界：idx0 起第一轮、idx4 起第二轮、idx8 起第三轮。
// 前两轮各带一次工具调用，用来验证 tool_use / tool_result 同生共死。
func buildTurns() []anthropic.MessageParam {
	return []anthropic.MessageParam{
		userText("北京天气怎么样？"),             // 0 ← 第一轮起点
		asstToolUse("t1", "get_weather"), // 1
		userToolResult("t1"),             // 2
		asstText("北京今天 22 度，晴。"),         // 3
		userText("那上海呢？"),                // 4 ← 第二轮起点
		asstToolUse("t2", "get_weather"), // 5
		userToolResult("t2"),             // 6
		asstText("上海 26 度，多云。"),          // 7
		userText("谢谢"),                   // 8 ← 第三轮起点
		asstText("不客气。"),                 // 9
	}
}

// ---------- 通用工具 ----------

func totalTokens(h []anthropic.MessageParam) int {
	total := 0
	for _, m := range h {
		total += estimateTokens(m)
	}
	return total
}

// firstText 取一条消息里第一个 text block 的文本，用于定点断言。
// 不用 reflect.DeepEqual 直接比 MessageParam：union 结构里有 nil/内部字段，
// 比出来又脆、失败信息又不可读。
func firstText(m anthropic.MessageParam) string {
	for _, b := range m.Content {
		data, err := json.Marshal(b)
		if err != nil {
			continue
		}
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(data, &p) == nil && p.Type == "text" {
			return p.Text
		}
	}
	return ""
}

// collectToolIDs 分别收集 tool_use 的 id 和 tool_result 的 tool_use_id。
// 坑：tool_result 的配对字段是 tool_use_id，不是 id。
func collectToolIDs(h []anthropic.MessageParam) (uses, results map[string]bool) {
	uses, results = map[string]bool{}, map[string]bool{}
	for _, m := range h {
		for _, b := range m.Content {
			data, err := json.Marshal(b)
			if err != nil {
				continue
			}
			var p struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			}
			if json.Unmarshal(data, &p) != nil {
				continue
			}
			switch p.Type {
			case "tool_use":
				uses[p.ID] = true
			case "tool_result":
				results[p.ToolUseID] = true
			}
		}
	}
	return uses, results
}

// turnCount 数一段历史里有几轮（首条算一轮起点，之后每条"新一轮提问"算一轮）。
func turnCount(h []anthropic.MessageParam) int {
	n := 0
	for i, m := range h {
		if i == 0 || isNewUserTurn(m) {
			n++
		}
	}
	return n
}

// checkInvariants 是滑动窗口的"合同"：任何输入、任何预算，输出都必须满足。
func checkInvariants(t *testing.T, in, out []anthropic.MessageParam, budget int) {
	t.Helper()

	// ① 非空：宁可超预算，也不把最新一轮切没
	if len(out) == 0 {
		t.Fatal("不变式①：结果为空 —— 底线是永不丢最新一轮")
	}

	// ② 裁剪后首条必须是新一轮提问；没裁剪（长度没变）时不做要求
	if len(out) < len(in) && !isNewUserTurn(out[0]) {
		types := make([]string, 0, len(out[0].Content))
		for _, b := range out[0].Content {
			types = append(types, blockType(b))
		}
		t.Errorf("不变式②：裁剪切在了轮中间，首条不是新一轮提问（role=%s, blocks=%v）",
			out[0].Role, types)
	}

	// ③ tool_use / tool_result 必须成对
	uses, results := collectToolIDs(out)
	for id := range uses {
		if !results[id] {
			t.Errorf("不变式③：tool_use %q 丢了配对的 tool_result", id)
		}
	}
	for id := range results {
		if !uses[id] {
			t.Errorf("不变式③：tool_result %q 丢了配对的 tool_use", id)
		}
	}

	// ④ 闭环：要么已经回到预算内，要么已经退到只剩最新一轮（后者允许超预算）。
	// 只断言 total <= budget 会在"只剩一轮但仍超预算"这个合法分支上误报；
	// 只断言"只剩一轮"又漏掉本该继续丢却没丢的 bug。
	if total := totalTokens(out); total > budget && turnCount(out) != 1 {
		t.Errorf("不变式④：total=%d 超预算 %d，却又不止一轮（未退到底线）", total, budget)
	}
}

// ---------- 表驱动：不变式回归 ----------

func TestTrimHistory(t *testing.T) {
	full := buildTurns()
	cases := []struct {
		name   string
		hist   []anthropic.MessageParam
		budget int
	}{
		{"空历史不 panic", nil, 100},
		{"远未超预算", full, totalTokens(full) * 3},
		{"恰好等于预算·不裁剪", full, totalTokens(full)},
		{"超一点·丢最旧一轮", full, totalTokens(full) - 1},
		{"刚好够后两轮·丢第一轮", full, totalTokens(full[4:])},
		{"不够两轮·只剩最新一轮", full, totalTokens(full[8:]) - 1},
		{"严重超·只剩最新一轮", full, 10},
		{"budget=0 不 panic", full, 0},
		{"负预算不 panic", full, -1},
		{"单轮超预算·宁超不切半轮", full[:4], 1},
		{"单轮零预算", full[:4], 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := TrimHistory(c.hist, c.budget)
			if len(c.hist) == 0 {
				if len(got) != 0 {
					t.Fatalf("空历史应返回空，实际 len=%d", len(got))
				}
				return
			}
			checkInvariants(t, c.hist, got, c.budget)
		})
	}
}

// ---------- 定点测试：钉住几个具体行为 ----------

// 边界：total == budget 属于"不超预算"，必须原样返回（判据是 <= 而不是 <）。
func TestTrimExactBudgetNoTrim(t *testing.T) {
	full := buildTurns()
	got := TrimHistory(full, totalTokens(full))
	if len(got) != len(full) {
		t.Fatalf("恰好等于预算不应裁剪：len(got)=%d, want %d", len(got), len(full))
	}
}

// 未超预算时返回原切片（零拷贝）。
// 这是当前的性能语义，不是正确性约束：若将来改成"总是新建切片"，删掉本测试即可。
func TestTrimUnderBudgetReturnsSameSlice(t *testing.T) {
	full := buildTurns()
	got := TrimHistory(full, totalTokens(full)*3)
	if len(got) != len(full) || &got[0] != &full[0] {
		t.Fatalf("未超预算应原样返回同一底层切片（零拷贝），实际 len=%d", len(got))
	}
}

// 丢的是"整轮"而不是"单条"：预算只差 1 个 token，也必须丢掉整个第一轮（4 条），
// 而不是只丢到最后刚好不超。
func TestTrimDropsWholeTurn(t *testing.T) {
	full := buildTurns()
	got := TrimHistory(full, totalTokens(full)-1)
	if len(got) != 6 {
		t.Fatalf("应丢掉整个第一轮：len(got)=%d, want 6", len(got))
	}
	if firstText(got[0]) != "那上海呢？" {
		t.Fatalf("窗口应停在第二轮起点，实际首条=%q", firstText(got[0]))
	}
}

// 底线：预算再小也只退到"只剩最新一轮"，绝不把最新一轮切掉。
func TestTrimKeepsLatestTurn(t *testing.T) {
	full := buildTurns()
	got := TrimHistory(full, 10)
	if len(got) != 2 {
		t.Fatalf("应只剩最新一轮（2 条）：len(got)=%d", len(got))
	}
	if firstText(got[0]) != "谢谢" {
		t.Fatalf("首条应是最后一轮的提问，实际=%q", firstText(got[0]))
	}
}

// 只有一轮时永远不裁剪：宁可超预算，也不切半轮
// （切在 tool_use 和它的 tool_result 中间，API 会 400）。
func TestTrimSingleTurnNeverTrimmed(t *testing.T) {
	one := buildTurns()[:4] // 单轮：提问 + tool_use + tool_result + 回答
	got := TrimHistory(one, 0)
	if len(got) != len(one) {
		t.Fatalf("单轮不应被裁剪：len(got)=%d, want %d", len(got), len(one))
	}
}

// 工具对不能被拆：预算刚好够后两轮时，
// 第一轮的 t1 必须"成对消失"，第二轮的 t2 必须"成对保留"。
func TestTrimNeverSplitsToolPair(t *testing.T) {
	full := buildTurns()
	got := TrimHistory(full, totalTokens(full[4:]))

	if len(got) != 6 {
		t.Fatalf("应保留后两轮（6 条），实际 len=%d", len(got))
	}
	uses, results := collectToolIDs(got)
	if uses["t1"] || results["t1"] {
		t.Errorf("第一轮被丢时 t1 应成对消失：uses=%v, results=%v", uses, results)
	}
	if !uses["t2"] || !results["t2"] {
		t.Errorf("第二轮的 t2 应成对保留：uses=%v, results=%v", uses, results)
	}
}
