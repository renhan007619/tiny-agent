// tiny-agent：最小可用 Agent 的 Go 实现（工具调用 + 短期记忆 + 上下文装配预算）。
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time" // 路线③：生成会话 id + GC 保留期

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
		//
		// 路线③ 新增 read_result，这段要写清三件事：
		//   1. 触发条件：看到"已外置…句柄 res-3"这种标记时
		//   2. 调用方式：read_result(handle, offset, limit) 按行翻页
		//   3. 禁止行为：不要说"内容不可用"，也不要猜被砍掉的部分
		// 少了第 3 条，模型很可能只回一句"内容被截断了"就停下。
		"You are Baz. Tools: get_color, get_number, read_file, read_result. "+
			"Use read_file when the user asks about a file's content. "+
			"Long tool results are truncated and externalized: you will see a marker like "+
			"[已外置：... 句柄 res-3 ...]. When you see such a marker, call "+
			"read_result(handle, offset, limit) to read the full content page by page. "+
			"Never claim the content is unavailable and never guess what was cut.",
	)

	budget := DefaultBudget()

	// --- 路线③：工具结果外置仓库 ---
	//
	// 会话 id 用"时间戳 + pid"：目录名可读（人工排查一眼知道是哪次），
	// 也不会和上一轮会话撞名。这里刻意不用随机串，就是为了"可读"。
	sessionID := fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), os.Getpid())

	// 第三个参数必须传 budget.ToolResultReserve：召回一页也是 tool_result，
	// 受同一条闸门约束。传进去 = 全项目只有一个事实来源（这条纪律在
	// maxTokens / historyBudgetTokens 上已经交过两次学费）。
	resultStore, err := NewResultStore(resultDirDefault, sessionID, budget.ToolResultReserve)
	if err != nil {
		// 构造期失败选择"直接退出"：它涉及磁盘布局（目录、权限），
		// 早失败比跑到一半发现写不进去好。
		// 注意区分：构造期失败要退出，运行期落盘失败只降级（见 externalizeToolResult）。
		fmt.Fprintln(os.Stderr, "初始化外置结果仓库失败", err)
		os.Exit(1)
	}

	// GC 放在"进程启动"而不是"每轮请求"：清理是 IO，不该进对话主路径；
	// 放启动也天然覆盖"上次崩了没来得及清"的情况。
	if n, cerr := resultStore.Cleanup(resultRetention); cerr != nil {
		// 清理失败不影响本次会话可用性——最坏结果是多占点磁盘，
		// 为它退出进程是不成比例的。
		fmt.Fprintln(os.Stderr, ">>> 清理外置结果失败:", cerr)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, ">>> 清理过期外置结果目录 %d 个\n", n)
	}
	llm.SetResultStore(resultStore)

	// 工具名用 snake_case：Go 函数名读出来是 camelCase（readFile），
	// 但工具名是给模型看的，snake_case 是生态惯例（Anthropic 官方示例也这么写）。
	//
	// ★ 顺序是硬要求：read_result 是仓库的**方法值**，tools 必须在仓库之后建，
	//   否则注册进去的是 nil 接收者，模型一调就 panic。
	readFileTool := newTool(readFile, "读取工作目录内的文本文件，返回全文；超大结果会被截断并外置")
	readFileTool.Name = "read_file"
	// 方法值推出来的名字是 "ReadResult"（首字母大写），必须显式覆盖成 snake_case，
	// 和 read_file 一样——忘了覆盖，模型看到的就是个不合惯例的工具名。
	readResultTool := newTool(resultStore.ReadResult, "按句柄读回被外置的完整工具结果，按行窗口读取（offset 从 1 开始）")
	readResultTool.Name = "read_result"
	tools := []*Tool{
		newTool(getColor, "随机返回一个颜色"),
		newTool(getNumber, "随机返回 0-100 的整数"),
		readFileTool,
		readResultTool,
	}
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
	// --- 启动横幅（简单 UI）---
	// 让用户知道 agent 就绪、用的什么模型/工具/哪次会话。工具名从 tools
	// 切片动态生成，不硬编码——systemPrompt 那处工具名耦合改不了，这里
	// 至少别再添一处。横幅走 stdout：它是 REPL 主体的一部分（进度/账本才走 stderr）。
	toolNames := make([]string, len(tools))
	for i, t := range tools {
		toolNames[i] = t.Name
	}
	fmt.Printf("tiny-agent 已就绪 | model=%s | session=%s\n工具: %s\n（Ctrl+Z 回车退出；设 TINY_AGENT_DEBUG_CONTEXT=1 可看装配账本）\n",
		model, sessionID, strings.Join(toolNames, ", "))

	ctx := context.Background()

	// --- 可观测性初始化 ---
	// 失败只降级、不阻断：agent 能不能跑比能不能观测重要。这里刻意不 os.Exit，
	// 也和"构造期失败要退出"（ResultStore）不同——那个是磁盘布局错误，这个只是
	// 没有 trace，业务功能完整。
	shutdown, terr := InitTelemetry(ctx)
	if terr != nil {
		fmt.Fprintln(os.Stderr, ">>> 可观测性初始化失败（本次运行无 trace）:", terr)
	} else {
		// 必须显式 Shutdown：Batch 默认 5s 落一次网，直接 return 会丢掉最后一批
		// span——包括"用户最后一句问话"那一整条 trace，而它往往正是你最想排查
		// 的那条。用独立超时 ctx：Shutdown 内部要发最后一个 batch 的 HTTP 请求，
		// 复用无超时的 ctx 会让后端挂掉时退出路径永远卡住。
		defer func() {
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer scancel()
			if err := shutdown(sctx); err != nil {
				fmt.Fprintln(os.Stderr, ">>> 关闭 tracer 失败（最后一批 span 可能丢失）:", err)
			}
		}()
	}

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
		// 每轮 60s 超时：LLM 请求挂起时（死代理/网关不吐响应），"无限卡住
		// 还零输出"是最差失败模式。超时把它变成显式报错，与其他 error 走
		// 同一条排查路径。
		askCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		// 每轮一个 root span：一次"任务"就是用户这一次输入。
		// 不按整个会话切 trace——会话可能几十分钟几十轮，trace 太长没法看；
		// 会话级关联走 session.id 属性。
		// 注意 askCtx 被重新赋值：这个 ctx 往下传，agent.go 里所有 span 才会
		// 成为它的子节点。ctx 不往下传 = 断链，这是 OTel 第一高频坑。
		askCtx, turnSpan := tracer().Start(askCtx, spanTurn, trace.WithAttributes(
			attribute.String(attrSessionID, sessionID),
			attribute.Int(attrHistoryMsgs, len(history)),
		))
		res, err := llm.SendMessage(askCtx, SendRequest{
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
		cancel() // SendMessage 已返回（无论成败），计时器立刻释放
		// span 必须在循环内显式 End：写成 defer 会累积到 main 返回，那样第 1 轮
		// 的 span 会把后面所有轮次的时间都算进去，耗时全错。
		recordErr(turnSpan, err)
		turnSpan.End()
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
