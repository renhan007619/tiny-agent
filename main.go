// tiny-agent：最小可用 Agent 的 Go 实现（工具调用 + 短期记忆 + 上下文装配预算）。
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
)

// 滑动窗口预算（按公式推导，假设窗口 128k）：输出预留 2048×6≈12k，
// 工具结果+system+本轮输入≈3k，再留安全余量 → ~90000。
// 若真实跑长对话报 context 超限，把此值调小一档即可。
// ============ 4. main：命令行 REPL（对话入口） ============
//
// 对应 agent.py 第 4 块（main 函数）。整体结构从上到下 4 块：
//  1. tools.go     工具函数（getColor/getNumber）
//  2. schema.go    工具转换层（函数 -> API 工具说明书）
//  3. agent.go     MinCore 核心循环
//  4. main.go      REPL 入口（history 即短期记忆，进程内有效）
func main() {
	loadDotEnv() // 读 .env 里的配置
	apiKey := os.Getenv("BAZ_OPENAI_API_KEY")
	baseURL := os.Getenv("BAZ_OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.lkeap.cloud.tencent.com/plan/anthropic"
	}
	model := os.Getenv("BAZ_OPENAI_MODEL")
	if model == "" {
		model = "deepseek-v4-flash-202605"
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "BAZ_OPENAI_API_KEY not set（先复制 .env.example 为 .env 并填入 key）")
		os.Exit(1)
	}

	// 创建 Agent：system prompt 告诉它自己是 Baz、有哪些工具、什么时候用
	llm := NewMinCore(
		apiKey,
		baseURL,
		model,
		// 加工具必须同步改这里：模型只从 system prompt 和 tools schema 知道
		// 自己有什么能力。只加 tools 不改 prompt，模型不会主动去用它——
		// 这是一处隐性耦合，加/删工具时最容易漏。
		"You are Baz. Tools: get_color, get_number, read_file. "+
			"Use read_file when the user asks about a file's content. "+
			"Large results are truncated and marked; tell the user when you see a truncation marker.",
	)
	// 工具名用 snake_case：Go 函数名读出来是 camelCase（readFile），
	// 但工具名是给模型看的，snake_case 是生态惯例（Anthropic 官方示例也这么写）。
	readFileTool := newTool(readFile, "读取工作目录内的文本文件，返回全文；超大结果会被截断")
	readFileTool.Name = "read_file"
	tools := []*Tool{
		newTool(getColor, "随机返回一个颜色"),
		newTool(getNumber, "随机返回 0-100 的整数"),
		readFileTool,
	}
	budget := DefaultBudget()
	store, err := NewStore("memory.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开记忆库失败", err)
		os.Exit(1)
	}
	defer store.Close()

	// --- REPL 循环 ---
	// history 是短期记忆的"外部持有者"：每轮对话结束后把更新后的
	// messages 存回 history，下一轮再传进去，任务状态才得以延续。
	// 注意：它只活在进程内存里——退出程序就全部丢失。
	ctx := context.Background()
	var history []anthropic.MessageParam
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n>>> You: ")
		if !scanner.Scan() {
			break // Ctrl+D / EOF 退出
		}
		text := scanner.Text()
		// 传回 history（短期记忆），拿回更新后的 history 和回答
		// 取料：检索是 IO，留在 REPL 层，装配器保持纯函数
		var memoryHits []string
		if hits, serr := store.Search(text, 3); serr == nil {
			memoryHits = hits
		}
		// 预算、裁剪、system 拼装全部在 SendMessage 内部完成
		res, err := llm.SendMessage(ctx, SendRequest{
			UserMessage: text,
			History:     history,
			Tools:       tools,
			MemoryHits:  memoryHits,
			Budget:      budget,
			// 默认关：① 稳定前缀才几十 token，低于官方缓存最小长度，收益≈0；
			// ② TokenHub 对 cache_control 的兼容未实测，风险>0。
			// 实测通过（不 400 且 usage 出现缓存计费字段）再开。
			EnableCache: false,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, ">>> error:", err)
			// 失败时的账本更有排查价值：到底装了多大、谁被砍了
			if os.Getenv("TINY_AGENT_DEBUG_CONTEXT") != "" {
				fmt.Fprintln(os.Stderr, res.Context.String())
			}
			continue
		}
		history = res.History
		fmt.Println("\n>>> Agent:", res.Reply)
		if os.Getenv("TINY_AGENT_DEBUG_CONTEXT") != "" {
			fmt.Fprintln(os.Stderr, res.Context.String())
		}

		// --- 长期记忆 · 写入（对话后） ---
		if aerr := store.Add("Q: " + text + " A: " + res.Reply); aerr != nil {
			fmt.Fprintln(os.Stderr, ">>> 记忆写入失败:", aerr)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "read error:", err)
	}
}
