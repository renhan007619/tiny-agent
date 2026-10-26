package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

// 整条链路（对着 InitTelemetry 从上往下读，正好一步步对上）：
//  1. 程序启动 → InitTelemetry：组装 Resource、采样器、处理器、导出器、传播器，注入全局 API
//  2. 业务代码 → otel.Tracer(...).Start(ctx, "span 名")，拿到的 ctx 必须继续往下传
//  3. SDK：采样器先决策采不采 → 采则建 span，自动附加 Resource 元数据
//  4. 业务结束 → span.End()
//  5. 处理器：Batch 攒批（生产）/ Syncer 立刻导出（本地调试）
//  6. 导出器：把 span 编码成 OTLP，发给后端或 Collector
//  7. 跨服务：传播器把 traceparent 注入请求头，下游解析后接回同一条 trace
//  8. 程序退出 → Shutdown：刷新剩余 span，优雅关闭
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

// annotateContext 把上下文账本摊成 span 属性。
// 别人的 trace 只能告诉你"这一轮花了多少 token"，这条能告诉你
// "钱花在哪一段、哪一段被裁了、有多少内容被移出了窗口"。
func annotateContext(span trace.Span, r AssembleReport) {
	ratio := 0.0
	if r.Available > 0 {
		ratio = float64(r.TotalUsed) / float64(r.Available)
	}
	span.SetAttributes(
		attribute.Int("context.window", r.Window),
		attribute.Int("context.output_reserve", r.OutputReserve),
		attribute.Int("context.available", r.Available),
		attribute.Int("context.message_budget", r.MessageBudget),
		attribute.Int("context.used", r.TotalUsed),
		// 水位线：单独一个浮点比，比在脑子里算除法快得多，
		// 也是将来做告警（水位连续 N 次 > 90%）的唯一输入。
		attribute.Float64("context.used_ratio", ratio),
	)
	for _, s := range r.Sections {
		k := "context.section." + sectionKey(s.Name)
		span.SetAttributes(
			attribute.Int(k+".used", s.Used),
			attribute.Int(k+".budget", s.Budget),
			attribute.Bool(k+".trimmed", s.Trimmed),
		)
	}
	// 外置量必须进 trace：它是"丢了 20649 token 无人记账"那个盲区的出口。
	// 用量是定值走属性，明细条数不定走 event（属性 key 必须固定）。
	if len(r.Externalized) > 0 {
		total := 0
		for _, e := range r.Externalized {
			total += e.Tokens
			span.AddEvent("context.externalized", trace.WithAttributes(
				attribute.String("tool.name", e.Tool),
				attribute.String(attrToolHandle, e.Handle),
				attribute.Int("tool.result.tokens", e.Tokens),
			))
		}
		span.SetAttributes(
			attribute.Int("context.externalized.tokens", total),
			attribute.Int("context.externalized.count", len(r.Externalized)),
		)
	}
	// 告警走 event 而不是属性：属性 key 必须固定，告警是"追加式"事实、
	// 条数不定；event 天生干这个（带时间点，不参与索引基数）。
	for _, w := range r.Warnings {
		span.AddEvent("context.warning", trace.WithAttributes(attribute.String("message", w)))
	}
}

// sectionKey 把账本里的中文段名映射成规范属性 key。
// 段名是给人看的（"system(记忆)"），属性 key 是给后端索引的，两者不该共用
// 一套字符串——中文和括号在部分后端会被丢弃或转义，属性"设了但查不到"。
func sectionKey(name string) string {
	switch name {
	case "tools":
		return "tools"
	case "system(稳定)":
		return "system_stable"
	case "system(记忆)":
		return "system_memory"
	case "history":
		return "history"
	case "user(本轮)":
		return "user"
	}
	// 兜底：非 ASCII 字母数字一律换下划线，保证属性 key 合法。
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			sb.WriteRune(r + ('a' - 'A'))
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// consoleExporter 把 span 直接打到 stderr，用于没有 Jaeger/Tempo 时验收。
// 不引官方 stdout exporter 的理由：① 不想为调试多引一个包；② 官方导出的
// 是 SDK 内部结构大 JSON，人读着痛苦；③ 输出格式要对齐上面那张属性表，
// 方便直接和账本对账。
// 关键是它和 Jaeger 实现的是同一个 SpanExporter 接口——换 exporter 不用改
// 任何一处埋点代码。这就是"管道与语义分离"最实在的证据。
type consoleExporter struct{}

func (consoleExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		fmt.Fprintf(os.Stderr, "=== [span] %-18s %10s  trace=%s\n",
			s.Name(),
			s.EndTime().Sub(s.StartTime()).Round(time.Microsecond),
			s.SpanContext().TraceID().String()[:8])
		if st := s.Status(); st.Code != codes.Unset {
			fmt.Fprintf(os.Stderr, "      ! status = %v: %s\n", st.Code, st.Description)
		}
		for _, a := range s.Attributes() {
			fmt.Fprintf(os.Stderr, "      %s = %v\n", a.Key, a.Value.Emit())
		}
		for _, e := range s.Events() {
			fmt.Fprintf(os.Stderr, "      event %s", e.Name)
			for _, a := range e.Attributes {
				fmt.Fprintf(os.Stderr, " %s=%v", a.Key, a.Value.Emit())
			}
			fmt.Fprintln(os.Stderr)
		}
	}
	return nil
}

func (consoleExporter) Shutdown(context.Context) error { return nil }
