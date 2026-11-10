package interview

// EventType 是事件的名字，用字符串而不是整数：
// 看日志的时候 "question.answered" 一眼能懂，数字 3 要看表。
// 类型别名再包一层，是为了让编译期能挡住"随手写个字符串"——
// 拼错名字会变成另一件事，而且是静默的。
type EventType string

// 全项目的事件只有这六个。不要加。
//
// 事件粒度是这类设计最容易失控的地方：
// 细到 "answer.typed" 就是事件风暴，粗到 "session.changed"
// 就是让所有人读全部内容再自己筛。六个够用了。
const (
	EvSessionStarted   EventType = "session.started"   // 开考
	EvQuestionAsked    EventType = "question.asked"    // 出了第几题
	EvQuestionAnswered EventType = "question.answered" // 第几题答完了
	EvGradeDone        EventType = "grade.done"        // 这题分数出来了
	EvGradeFailed      EventType = "grade.failed"      // 重试用尽也没打上
	EvSessionFinished  EventType = "session.finished"  // 这场结束了
)

// Event 是一张便签：只写"发生了什么"，不搬数据。
//
// 【必记】这里没有 Payload any 这种大口袋，字段都是具体的。
// 两个理由：
//  1. 类型安全——接收方拿到 ev.Idx 就是 int，不用做类型断言
//  2. 防走偏——有了 any，早晚会有人往事件里塞整段回答，
//     于是库里一份、事件里一份，两份数据不一样的时候你不知道信谁
//
// 要业务数据就去库里读，凭 SessionID + Idx 一定能读到。
// 事件是"该去看一眼"的通知，不是数据搬运工。
type Event struct {
	Type      EventType
	SessionID string
	Idx       int   // 第几题（1 开始）；跟具体题目无关的事件填 0
	Err       error // 只有 failed 类用它：这次为什么没成
}

// Err 是唯一一个"不进库"的字段，因为它本来就不是状态，
// 是一次性的诊断信息。而且 error 没法 JSON 化，正合适——
// 想落库也落不进去，只能当场用掉。
