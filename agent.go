package main

import (
	"context"
	"encoding/json"
	"errors"
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

// callModel 一次 Messages API 调用。
func (m *MinCore) callModel(ctx context.Context, messages []anthropic.MessageParam, apiTools []anthropic.ToolUnionParam) (*anthropic.Message, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: m.maxTokens,
		Messages:  messages,
	}
	if m.systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: m.systemPrompt}}
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
func (m *MinCore) SendMessage(ctx context.Context, userMessage string, history []anthropic.MessageParam, tools []*Tool, maxRounds int) ([]anthropic.MessageParam, string, error) {
	// 本次请求的工具说明书只生成一次（对应 Python: schemas = make_schemas(funcs)）
	schemas := m.apiTools(tools)

	// --- 短期记忆写入（1/3）：追加用户消息 ---
	// messages 就是短期记忆：记录"这次任务从头到尾发生了什么"
	messages := history
	if messages == nil {
		messages = []anthropic.MessageParam{}
	}
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)))

	fnMap := makeToolMap(tools) // 名字 -> 工具（对应 Python: make_function_map）

	// --- 工具循环：最多 maxRounds 轮 ---
	for range maxRounds {
		// 1) 调模型
		resp, err := m.callModel(ctx, messages, schemas)
		if err != nil {
			return messages, "", err
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
			return messages, sb.String(), nil
		}

		// 4) 短期记忆写入（2/3）：助手这轮完整内容原样放回历史
		messages = append(messages, anthropic.NewAssistantMessage(assistantContent(resp.Content)...))

		// 5) 逐个执行模型请求的工具
		var results []anthropic.ContentBlockParamUnion
		for _, c := range calls {
			tool := fnMap[c.name]
			if tool == nil {
				// 模型要求了不存在的工具（幻觉/名字写错）——明确报错并标 is_error，
				// 让模型知道自己错了（比 Python 版只回文本更严谨的一处）
				results = append(results, anthropic.NewToolResultBlock(c.id, "unknown function: "+c.name, true))
				continue
			}
			out, execErr := tool.execute(c.input)
			if execErr != nil {
				results = append(results, anthropic.NewToolResultBlock(c.id, "tool error: "+execErr.Error(), true))
				continue
			}
			// 每个工具调用都要有对应的 tool_result，用 tool_use_id 关联
			results = append(results, anthropic.NewToolResultBlock(c.id, stringify(out), false))
		}

		// 6) 短期记忆写入（3/3）：工具结果回填
		// Anthropic API 规定：tool_result 消息的 role 必须写 "user"
		messages = append(messages, anthropic.NewUserMessage(results...))
		// 回到循环顶部，再次检查模型要不要继续调工具
	}

	return messages, "", errors.New("工具循环超过最大轮数")
}
