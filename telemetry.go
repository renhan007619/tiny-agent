package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
)

// 这个文件只放两样东西：基建（provider / exporter / 传播器）和语义约定
// （span 名、属性 key）。真正的埋点长在业务发生的地方（agent.go 的工具循环、
// main.go 的 REPL）——因为可观测性不落在某一层，它横穿所有层。
//
// 三条取舍：
//  1. 管道不造：OTLP + BatchSpanProcessor + W3C 传播器，后端可换。
//  2. 语义自己定：gen_ai.* 沿用 OTel GenAI 语义约定，agent 特有的用
//     context.* / tool.* 扩展；属性 key 全部集中在本文件。
//  3. 内容不进属性：prompt 和工具结果全文绝不进 span，只记长度和句柄。

const (
	tracerName = "tiny-agent"
	svcVersion = "0.1.0"
)

// span 名。一处定义，避免"这里叫 gen_ai.chat、那里叫 chat"的口径分裂
// （maxTokens 和 historyBudget 上已经为同类问题交过学费）。
const (
	spanTurn     = "agent.turn"       // 一轮用户输入（root span）
	spanAssemble = "context.assemble" // 一次上下文装配
	spanChat     = "gen_ai.chat"      // 一次 LLM 调用（对齐 GenAI 语义约定）
	spanTool     = "execute_tool"     // 一次工具执行
)

// 属性 key。集中管理 = semconv 改名时只改这一张表，业务代码不动。
const (
	// GenAI 语义约定：通用语言，后端认了才能自动算出 token 成本、模型分布面板。
	attrModel        = "gen_ai.request.model"
	attrInputTokens  = "gen_ai.usage.input_tokens"
	attrOutputTokens = "gen_ai.usage.output_tokens"
	attrCacheRead    = "gen_ai.usage.cache_read.input_tokens"
	attrCacheWrite   = "gen_ai.usage.cache_creation.input_tokens"
	attrToolCalls    = "gen_ai.response.tool_calls"

	// agent 扩展：官方约定没覆盖到的地方，按同样风格自己扩。
	attrRound          = "agent.round"
	attrSessionID      = "session.id"
	attrHistoryMsgs    = "history.messages"
	attrCacheEnabled   = "context.cache_enabled"
	attrToolName       = "tool.name"
	attrToolInputBytes = "tool.input.bytes"
	attrToolResBytes   = "tool.result.bytes"
	attrToolResTokens  = "tool.result.tokens"
	attrToolTruncated  = "tool.result.truncated"
	attrToolHandle     = "tool.result.handle"
)

func InitTelemetry(ctx context.Context) (func(context.Context) error, error) {
	// 端点交给标准环境变量 OTEL_EXPORTER_OTLP_ENDPOINT（默认 localhost:4318），
	// 代码里不硬编码后端地址——换后端是配置问题，不是代码问题。
	// 超时调短：后端没起时别让导出干等到默认超时。
	useConsole := os.Getenv("TINY_AGENT_TRACE_STDOUT") != ""
	var (
		exp sdktrace.SpanExporter
		err error
	)
	if useConsole {
		exp = consoleExporter{} // 没有 Jaeger 时也能验收
	} else {
		exp, err = otlptracehttp.New(ctx, otlptracehttp.WithTimeout(3*time.Second))
		if err != nil {
			return nil, fmt.Errorf("初始化 OTLP exporter: %w", err)
		}
	}

	// WithFromEnv 在前（OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME），
	// WithAttributes 在后 = 显式属性优先，不会被环境变量意外覆盖。
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", "tiny-agent"),
			attribute.String("service.version", svcVersion),
			attribute.String("deployment.environment.name", "dev"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("构造 resource: %w", err)
	}

	// 采样：本地先 100% 全采（调试要看到每一条）。
	// agent 场景的坑：失败请求往往更慢更长，而头部采样（先掷骰子）会
	// 系统性丢掉失败样本。上生产要么降比例，要么在 Collector 侧做尾采样。
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))

	// Batch 而不是 Simple：Simple 每次 span.End() 同步导一次网络往返，
	// 一条 trace 十几个 span 就是十几次往返，直接把工具循环拖慢。
	// 代价：Batch 默认 5s/512 条落一次，进程退出不 Shutdown 就丢最后一批。
	// 本地调试（console）反而要 Simple：等 5s 才看到输出没法调试。
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	if useConsole {
		opts = append(opts, sdktrace.WithSyncer(exp))
	} else {
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	// W3C 传播器：现在全单进程，看着没用；但工具一旦开始发 HTTP 请求，
	// traceparent 要靠它带出去，否则下游服务在 trace 里就是孤儿节点。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	// 默认全局错误处理器会把每次导出失败都打到 stderr（每个 batch 一次）。
	// 后端没起时 REPL 会被 "connection refused" 刷屏——可观测性自己不能
	// 成为噪音源。只保留首次提示。
	var errOnce sync.Once
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		errOnce.Do(func() {
			fmt.Fprintln(os.Stderr,
				">>> 可观测性导出失败（后续同类错误静默；检查 OTEL_EXPORTER_OTLP_ENDPOINT 与后端是否启动）:", err)
		})
	}))

	return tp.Shutdown, nil
}

// 1. **程序启动**：调用 `InitTelemetry` → 组装 SDK、采样、处理器、导出器、传播器 → 注入全局 API
// 2. **请求进来**：业务代码调用 `otel.Tracer("service").Start(ctx, "span名")`
// 3. **SDK 内部**：采样器决策是否采样 → 采样则创建 span，自动附加 Resource 元数据
// 4. **业务执行**：逻辑结束后调用 `span.End()`
// 5. **打包处理**：span 交给处理器 → 批量模式攒批 / 同步模式立刻导出
// 6. **导出发送**：导出器把 span 编码成 OTLP 格式 → 发给后端 / Collector
// 7. **跨服务调用**：传播器把 trace 上下文注入请求头 → 下游解析恢复链路
// 8. **程序退出**：调用 `shutdown` → 刷新剩余 span，优雅关闭
// sampleRatio 读采样率，非法值一律退回 1.0（宁可多采，不要静默丢样本）。
func sampleRatio() float64 {
	v := os.Getenv("TINY_AGENT_TRACE_SAMPLE")
	if v == "" {
		return 1.0
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil || r < 0 || r > 1 {
		fmt.Fprintf(os.Stderr, ">>> 忽略非法的 TINY_AGENT_TRACE_SAMPLE=%q（按 1.0 处理）\n", v)
		return 1.0
	}
	return r
}

// tracer 取全局 tracer。otel.Tracer 走 delegate 机制，即使在
// SetTracerProvider 之前调用也不报错，所以不需要"注入"。
func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// recordErr 把错误记进 span。两件事都要做：
//   - RecordError 记一条 error event（保留堆栈和原始信息）
//   - SetStatus(Error) 改 span 状态（后端靠状态做错误率统计和采样决策）
//
// 只做前者，后端看不出这条 trace 是失败的；只做后者，丢了错误详情。
func recordErr(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
