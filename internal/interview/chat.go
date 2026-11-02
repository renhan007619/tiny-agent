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
		// 2048 而不是 1024：deepseek-v4-flash 这类推理模型会先写思考过程，
		// 思考块和正文块共用同一个额度。给 1024 时思考一旦写长，正文就是空的，
		// 表现成"模型一个字都没说"（实测 15 条用例里挂了 3 条）。
		MaxTokens: 2048,
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
		return "", fmt.Errorf("模型一个字都没说")
	}
	return strings.TrimSpace(sb.String()), nil
}
