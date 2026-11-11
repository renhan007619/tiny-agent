package interview

import (
	"context"
	"fmt"
)

// Evaluator 把打分器包装成一个能接事件的 agent。
//
// 【必记】它还是个 worker，不是 agent。
// 收到事件就干活，没有决策、没有循环、没有自己的目标。
// 包这一层只改了一件事：从"main 直接调它"变成"它自己订阅事件"。
type Evaluator struct {
	grader *Grader
	store  *Store
}

// NewEvaluator 把已有的打分器接到总线上。
func NewEvaluator(g *Grader, s *Store) *Evaluator {
	return &Evaluator{grader: g, store: s}
}

func (e *Evaluator) Name() string { return "evaluator" }

func (e *Evaluator) Handles() []EventType {
	return []EventType{EvQuestionAnswered}
}

// Handle 收到"第 N 题答完了"，回库里读这一题，交给原来的打分队列。
//
// Submit 里的判断（反问跳过、空答案不评、塞队列）一个字没改——
// 接线时不该顺手改业务逻辑，否则出问题分不清是哪边造成的。
//
// 这里也不做慢活：Submit 只是把任务塞进打分队列就返回，
// 真正调模型的是打分器自己的 worker。总线不许被一道题卡住。
//
// 【必记】这里有条隐含的时序依赖：先落库、再发事件。
// main 里是 SaveAnswer 在前、Publish 在后，所以这里读得到。
// 哪天有人把顺序调过来，这里会读到空答案——
// 而且不报错，只会"莫名其妙少打一题分"。
func (e *Evaluator) Handle(ctx context.Context, ev Event) error {
	t, err := e.store.LoadTurn(ev.SessionID, ev.Idx)
	if err != nil {
		return fmt.Errorf("读第 %d 题失败: %w", ev.Idx, err)
	}
	e.grader.Submit(t)
	return nil
}
