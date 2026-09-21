package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// ============ 3. MinCore：Agent 核心（tool_use / tool_result 循环） ============
//
// 对应 agent.py 第 3 块（MinCore 类）。思想完全一致：
//   发消息 -> 模型要调工具 -> 执行工具 -> 回填 tool_result -> 再发消息，
//   直到模型给出最终文本回答。
// 对话历史（messages = 短期记忆）由调用方持有并传回，本类不保存状态。

// MinCore 最小 Agent 核心。
type MinCore struct {
	client       anthropic.Client // 注意：NewClient 返回值类型（不是指针）
	model        string
	systemPrompt string
	maxTokens    int64
}

// NewMinCore 初始化核心。
// - client：官方 Go SDK（base_url 用 option.WithBaseURL 指向腾讯 TokenHub 中转）
// - model：deepseek-v4-flash-202605
// - systemPrompt：固定背景指令，每轮请求都会带上
func NewMinCore(apiKey, baseURL, model, systemPrompt string) *MinCore {
	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
	)
	return &MinCore{
		client:       client,
		model:        model,
		systemPrompt: systemPrompt,
		maxTokens:    2048, // 单次回答的最大 token 数（防止无限生成）
	}
}

// apiTools 把我们的 Tool 列表转成 SDK 认识的工具说明书 []anthropic.ToolUnionParam。
// 对应 Python 的 make_schemas。
func (m *MinCore) apiTools(tools []*Tool) []anthropic.ToolUnionParam {
	api := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		// Schema 由 tools.go 自己构造、结构已知：断言失败只会得到 nil 映射/切片，
		// 效果等价于"这个工具没有参数"，所以第二个返回值可以安全忽略。
		props, _ := t.Schema["properties"].(map[string]any)
		req, _ := t.Schema["required"].([]string)
		api = append(api, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: param.NewOpt(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: props,
				Required:   req,
			},
		}})
	}
	return api
}

// Reart：为什么需要SendRequest？因为在之前上下文装配器需要的原料横跨main.go和agent.go,这里选择弄成struct，在sendmessage使用
type SendRequest struct {
	UserMessage string
	History     []anthropic.MessageParam // 之前的对话历史（不含本轮）
	Tools       []*Tool
	MemoryHits  []string // 长期记忆命中，由调用方检索好传进来（见 main.go）
	Budget      Budget
	EnableCache bool
}

// SendResult 一次对话请求的结果。
type SendResult struct {
	History []anthropic.MessageParam // 更新后的历史，调用方存好下轮回传
	Reply   string
	Context AssembleReport // 本次上下文装配账本（可观测性）
}

// callModel 一次 Messages API 调用。
func (m *MinCore) callModel(ctx context.Context, messages []anthropic.MessageParam, apiTools []anthropic.ToolUnionParam, system []anthropic.TextBlockParam) (*anthropic.Message, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: m.maxTokens,
		Messages:  messages,
	}
	if len(system) > 0 {
		params.System = system
	}
	if len(apiTools) > 0 {
		params.Tools = apiTools
	}
	return m.client.Messages.New(ctx, params)
}

// assistantContent 把响应 content 原样转回参数（assistant 消息回填用）。
// 思想同 agent.py 第 117 行注释：assistant 这轮完整内容（含 tool_use 块）
// 必须放回历史，模型下一轮才能看到"自己上一步要求调了哪些工具"。
// 响应类型 []ContentBlockUnion 和请求类型 []ContentBlockParamUnion 是
// 两套结构（SDK 设计），所以要逐个 block 转回去——这就是 Go 的"类型体操"。
func assistantContent(content []anthropic.ContentBlockUnion) []anthropic.ContentBlockParamUnion {
	var blocks []anthropic.ContentBlockParamUnion
	for _, b := range content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			blocks = append(blocks, anthropic.NewTextBlock(v.Text))
		case anthropic.ToolUseBlock:
			// ToolUseBlock.Input 是 json.RawMessage，原样塞回去
			blocks = append(blocks, anthropic.NewToolUseBlock(v.ID, v.Input, v.Name))
		}
	}
	return blocks
}

// SendMessage 发一条用户消息，自动执行工具调用循环。
//
// 参数：
//   - userMessage：用户本轮输入
//   - history：之前的对话历史（短期记忆！），nil 表示新会话
//   - tools：本次可用的工具列表
//   - maxRounds：工具循环的最大轮数（防止模型无限调工具）
//
// 返回：(更新后的历史, 最终文本)
// 调用方要把"更新后的历史"存好，下次再传回来，任务状态才延续。
func (m *MinCore) SendMessage(ctx context.Context, req SendRequest) (SendResult, error) {
	// 只兜"完全没给预算"（全零结构体）。半给（如只填了 Window）是调用方 bug：
	// 兜底会把它藏成"工具循环静默不执行"的灵异现象，不如让它显式暴露。
	if req.Budget == (Budget{}) {
		req.Budget = DefaultBudget()
	}
	// maxTokens 单一事实来源：真正发出去的是 m.maxTokens，预算必须按它算。
	// 没有这行，两边各写一个 2048、将来改一处漏一处，输出预留就漂了。
	req.Budget.MaxTokens = int(m.maxTokens)

	// 工具说明书只生成一次：它同时是"要发出去的东西"和"要记账的东西"，
	// 口径唯一才不会出现"估的是一套、发的是另一套"。
	// 本次请求的工具说明书只生成一次（对应 Python: schemas = make_schemas(funcs)

	schemas := m.apiTools(req.Tools)

	// --- 短期记忆写入（1/3）：追加用户消息 ---
	// messages 就是短期记忆：记录"这次任务从头到尾发生了什么"

	asm := AssembleContext(AssembleInput{
		BaseSystem:  m.systemPrompt,
		MemoryHits:  req.MemoryHits,
		History:     req.History,
		UserInput:   req.UserMessage,
		Tools:       schemas,
		Budget:      req.Budget,
		EnableCache: req.EnableCache,
	})
	res := SendResult{Context: asm.Report}
	messages := asm.Messages

	fnMap := makeToolMap(req.Tools) // 名字 -> 工具（对应 Python: make_function_map）

	// 工具循环里 messages 会涨（每轮 +assistant(tool_use) +user(tool_result)），
	// 这部分增长装配时只能"预留"不能"预知"，所以每轮调用前复核账本。三个要点：
	//   ① 比 MessageBudget（messages 自己的配额），不是 Available（整个内容池）——
	//      拿段比池子，得等 messages 把 tools/system 的份额都吃掉才响，护栏等于没有；
	//   ② 第 1 轮不比：那时 messages 就是装配结果，必然不超；真是装配期就超了，
	//      AssembleContext 已经报过（固定段吃穿内容池 / history 宁超不切半轮），
	//      重复报没有信息量。有增量信息的只有第 2 轮起——tool_result 才是
	//      装配时看不见的那部分；
	//   ③ 告警必须默认可见：只塞进 res.Context.Warnings，没开
	//      TINY_AGENT_DEBUG_CONTEXT 就完全静默——护栏静默等于不存在。
	// 超了只告警、不裁剪：按需截断超长工具结果是路线图②（L0，无需 LLM），这里只做观测。
	warned := false
	for round := range req.Budget.MaxToolRounds {
		if !warned && round > 0 {
			if used := historyTokensOf(messages); used > asm.Report.MessageBudget {
				warned = true
				msg := fmt.Sprintf(
					"工具循环第 %d 轮：messages 已涨到 %d token，超出配额 %d（内容池可用 %d，含 tools/system/记忆）；本次请求可能被 API 以超长拒掉",
					round+1, used, asm.Report.MessageBudget, asm.Report.Available)
				res.Context.Warnings = append(res.Context.Warnings, msg)
				fmt.Fprintln(os.Stderr, ">>> warning:", msg)
			}
		}

		// --- 工具循环：最多 maxRounds 轮 ---
		// 1) 调模型
		resp, err := m.callModel(ctx, messages, schemas, asm.System)
		if err != nil {
			res.History = messages
			return res, err
		}

		// 2) 收集本轮模型要求的工具调用
		type call struct {
			id, name string
			input    json.RawMessage
		}
		var calls []call
		for _, b := range resp.Content {
			if v, ok := b.AsAny().(anthropic.ToolUseBlock); ok {
				calls = append(calls, call{id: v.ID, name: v.Name, input: v.Input})
			}
		}

		// 3) 模型没要求调工具 -> 准备直接回答，退出循环
		if len(calls) == 0 {
			var sb strings.Builder
			for _, b := range resp.Content {
				if v, ok := b.AsAny().(anthropic.TextBlock); ok {
					sb.WriteString(v.Text)
				}
			}
			res.History = messages
			res.Reply = sb.String()
			return res, nil
		}

		// 4) 短期记忆写入（2/3）：助手这轮完整内容原样放回历史
		messages = append(messages, anthropic.NewAssistantMessage(assistantContent(resp.Content)...))

		// 5) 逐个执行模型请求的工具
		// 26年9月20日 起：超长结果按配额截断（L0，零 LLM 成本）
		quota := toolResultQuota(req.Budget.ToolResultReserve, len(calls))
		var results []anthropic.ContentBlockParamUnion
		for _, c := range calls {
			tool := fnMap[c.name]
			if tool == nil {
				// 模型要求了不存在的工具（幻觉/名字写错）——明确报错并标 is_error，
				// 让模型知道自己错了（比 Python 版只回文本更严谨的一处）
				results = append(results, anthropic.NewToolResultBlock(c.id, "unknown function: "+c.name, true))
				continue
			}
			// 真正执行工具。注意这两步不能省：没有 execute，模型永远收不到工具结果；
			// 执行失败也必须回一条 is_error 的 tool_result，否则这次调用会悬空。
			out, execErr := tool.execute(c.input)
			if execErr != nil {
				results = append(results, anthropic.NewToolResultBlock(c.id, "tool error: "+execErr.Error(), true))
				continue
			}
			raw := stringify(out)
			text, cut := truncateToolResult(raw, quota)
			if cut {
				// 截断必须留痕：静默截断等于数据损坏，模型和人都该知道这条不完整。
				// 与工具循环护栏同一待遇（进报告 + 打 stderr），理由也一样——
				// 只写在报告里、没开 TINY_AGENT_DEBUG_CONTEXT 就完全静默。
				msg := fmt.Sprintf("工具 %s 结果约 %d token，超单条配额 %d，已截断（原始内容不可恢复，需完整内容请走外置+召回）",
					c.name, estimateTextTokens(raw), quota)
				res.Context.Warnings = append(res.Context.Warnings, msg)
				fmt.Fprintln(os.Stderr, ">>> warning:", msg)
			}
			// 每个工具调用都要有对应的 tool_result，用 tool_use_id 关联
			results = append(results, anthropic.NewToolResultBlock(c.id, text, false))
		}

		// 6) 短期记忆写入（3/3）：工具结果回填
		// Anthropic API 规定：tool_result 消息的 role 必须写 "user"
		messages = append(messages, anthropic.NewUserMessage(results...))
		// 回到循环顶部，再次检查模型要不要继续调工具
	}
	res.History = messages
	return res, errors.New("工具循环超过最大轮数")
}
