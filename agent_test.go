package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// 非交互测试：验证工具调用闭环（真实 API，需要 .env 里有 key）。
// 对应 test_agent.py：单工具 / 双工具两个场景。
// 运行：go test -v ./...  （首次联网调用模型）
func TestToolLoop(t *testing.T) {
	loadDotEnv()
	apiKey := os.Getenv("BAZ_OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("BAZ_OPENAI_API_KEY not set，跳过真实 API 测试")
	}
	baseURL := os.Getenv("BAZ_OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.lkeap.cloud.tencent.com/plan/anthropic"
	}
	model := os.Getenv("BAZ_OPENAI_MODEL")
	if model == "" {
		model = "deepseek-v4-flash-202605"
	}

	llm := NewMinCore(
		apiKey,
		baseURL,
		model,
		"You are Baz. You have two tools: get_color and get_number. "+
			"Use them when asked for colors or numbers.",
	)
	tools := []*Tool{
		newTool(getColor, "随机返回一个颜色"),
		newTool(getNumber, "随机返回 0-100 的整数"),
	}

	ctx := context.Background()
	var history []anthropic.MessageParam

	// 场景 1：单工具调用；场景 2：双工具调用（验证历史延续 + 一次多工具）
	for _, q := range []string{"give me a color", "now give me a color and a number"} {
		t.Log("Q:", q)
		var reply string
		var err error
		history, reply, err = llm.SendMessage(ctx, q, history, tools, 5)
		if err != nil {
			t.Fatalf("SendMessage(%q): %v", q, err)
		}
		t.Log("A:", reply)
		if strings.TrimSpace(reply) == "" {
			t.Fatalf("SendMessage(%q): 空回答", q)
		}
	}
}
