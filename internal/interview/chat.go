package interview

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// 说话的人是谁。面试官说的算 assistant，候选人说的算 user。
const (
	RoleAssistant = "assistant"
	RoleUser      = "user"
)

// Message 是一句话，只有文字，不带工具。
type Message struct {
	Role string
	Text string
}

// Chatter 是"跟模型说话"这件事的接口。
//
// 做成接口是为了能换掉它：测试的时候塞一个假人进去，
// 不花钱、不联网、也不会因为模型今天抽风就红。
type Chatter interface {
	Ask(ctx context.Context, systemPrompt string, history []Message) (string, error)
}

// ClaudeChatter 走 Anthropic 兼容接口说话（和 tiny-agent 用同一套配置）。
type ClaudeChatter struct {
	client anthropic.Client
	model  string
}

// NewClaudeChatter 建一个会说人话的面试官。
func NewClaudeChatter(apiKey, baseURL, model string) *ClaudeChatter {
	return &ClaudeChatter{
		client: anthropic.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
		),
		model: model,
	}
}

// Ask 发一句话过去，拿一句话回来。
func (c *ClaudeChatter) Ask(ctx context.Context, systemPrompt string, history []Message) (string, error) {
	msgs := make([]anthropic.MessageParam, 0, len(history)+1)
	for _, h := range history {
		if h.Role == RoleAssistant {
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(h.Text)))
		} else {
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(h.Text)))
		}
	}
	// 第一句时历史是空的，但接口要求至少有一条消息，
	// 所以塞一句开场信号进去，模型只负责开口。
	if len(msgs) == 0 {
		msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock("（面试开始）")))
	}

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model(c.model),
		// 8192 而不是 1024 或 2048：deepseek-v4-flash 是推理模型，
		// 思考块和正文块共用这一个额度，而且是「写满为止」——
		// 给 1024 时实测 15 条挂 3 条，给 2048 时判卷任务 31 条挂 23 条，
		// stop_reason 都是 max_tokens、返回块只有 thinking、正文 0 字。
		// 判卷提示词越细，它想得越长，所以这个数不能贴着当前实测值给。
		MaxTokens: 8192,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  msgs,
	})
	if err != nil {
		return "", fmt.Errorf("调模型失败: %w", err)
	}

	var sb strings.Builder
	for _, b := range resp.Content {
		if string(b.Type) == "text" {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		// 空返回有三种死法，光看"没说话"分不出来是哪一种：
		//   思考把额度吃光了（stop_reason=max_tokens，正文 0 字）
		//   中转给了个空壳（块数是 0）
		//   只回了 thinking 块、没有 text 块（块数不是 0 但类型对不上）
		// 不把这几个数带出来，下次看到同一句话还是不知道往哪查。
		return "", fmt.Errorf("模型一个字都没说（stop_reason=%s 输出=%d token 返回=%d 块 类型=%s 输入=%d token）",
			resp.StopReason, resp.Usage.OutputTokens, len(resp.Content), blockTypes(resp.Content), resp.Usage.InputTokens)
	}
	return strings.TrimSpace(sb.String()), nil
}

// blockTypes 把"这次到底回了什么块"列出来，比如 [thinking text]。
func blockTypes(blocks []anthropic.ContentBlockUnion) string {
	if len(blocks) == 0 {
		return "[]"
	}
	names := make([]string, 0, len(blocks))
	for _, b := range blocks {
		names = append(names, string(b.Type))
	}
	return "[" + strings.Join(names, " ") + "]"
}
