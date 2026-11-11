package interview

import (
	"context"
	"log"
)

// Logger 把总线上流过的事件原样打一行。
//
// 它不是日志框架，是这次改造的验收工具：
// 加它只需要在 main 里多一行 Subscribe，总线、打分器、main 的流程
// 一个字都不用改。这就是总线存在的理由。
type Logger struct{}

func NewLogger() *Logger { return &Logger{} }

func (l *Logger) Name() string { return "logger" }

// Handles 六个全订：这个组件要看完整流水，它不挑食。
//
// 六个都写上，是为了将来事件发全了它自动都有，不用回头改它。
// 这也是"订阅表"比"在分发处写 if 判断"好的地方：
// 分发那边完全不用认识 Logger。
func (l *Logger) Handles() []EventType {
	return []EventType{
		EvSessionStarted,
		EvQuestionAsked,
		EvQuestionAnswered,
		EvGradeDone,
		EvGradeFailed,
		EvSessionFinished,
	}
}

// Handle 只做一件事：打一行。
//
// 它必须是"快"的——纯打印，不碰数据库、不调模型。
// 总线是单 goroutine 分发的，任何订阅者慢下来都会堵住后面所有人，
// 所以订阅者的第一纪律是：要么立刻返回，要么把活扔进自己的队列。
func (l *Logger) Handle(_ context.Context, ev Event) error {
	if ev.Err != nil {
		log.Printf("[logger] %-18s session=%s idx=%d err=%v",
			ev.Type, ev.SessionID, ev.Idx, ev.Err)
		return nil
	}
	log.Printf("[logger] %-18s session=%s idx=%d", ev.Type, ev.SessionID, ev.Idx)
	return nil
}
