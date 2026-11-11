package interview

import (
	"context"
	"log"
	"sync"
)

// busBufferSize 是公告栏能暂存多少张便签。
//
// 满了会阻塞发便签的人——这不是缺陷，是故意的。
// 叫背压：生产者自动跟消费者同速，不会出现
// "你答的这题被静默丢掉了，而且没人告诉你"。
const busBufferSize = 64

// Bus 是公告栏：一个 channel + 一张订阅表。
//
// 它只管两件事：把便签收下来、分给关心它的人。
// 不负责并发（Run 是单 goroutine，顺序分发），
// 不负责重试（错了就是错了），不负责持久化（事件不落库）。
type Bus struct {
	ch   chan Event
	subs map[EventType][]Agent
	mu   sync.RWMutex // 护着 subs 和 ack

	// ack 是"挡板到了"的回执。
	//
	// Drain 塞挡板之前，先把一个 channel 存这儿；
	// Run 处理到挡板时把它关掉，Drain 于是知道"前面的都处理完了"。
	//
	// 【必记】为什么非得要这个回执：
	// 往 channel 里发送成功，只说明元素进了缓冲区，不代表有人取走。
	// 没有回执，Drain 塞完挡板就得立刻返回——那等于什么都没等。
	// 正向通道传"要处理的事"，反向通道传"处理完了"这一个信号，缺一不可。
	ack chan struct{}
}

// NewBus 造一个公告栏。
func NewBus() *Bus {
	return &Bus{
		ch:   make(chan Event, busBufferSize),
		subs: make(map[EventType][]Agent),
	}
}

// Subscribe 让一个 agent 挂上它关心的那几类事件。
//
// 同一个 agent 重复订阅同一类事件会被跳过。
// 为什么要管这个：重复订阅会让同一条事件被处理两次，
// 而"弱点次数 +2"这种 bug 事后极难查——看着像模型抽风。
// 这里挡住最省事。
func (b *Bus) Subscribe(a Agent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, t := range a.Handles() {
		if b.has(a, t) {
			continue
		}
		b.subs[t] = append(b.subs[t], a)
	}
}

// has 判断某个 agent 是不是已经订阅了这类事件。
// 用 Name() 当身份标识——所以 Name 必须唯一，这是它的硬要求。
func (b *Bus) has(a Agent, t EventType) bool {
	for _, x := range b.subs[t] {
		if x.Name() == a.Name() {
			return true
		}
	}
	return false
}

// Publish 贴一张便签，贴完就走。
//
// 阻塞式：channel 满了就在这里等，等不到就等 ctx 取消。
// 加上 ctx 是关键——裸的阻塞在退出时会永久卡死，
// 有了它，Ctrl+C 能让所有等待立刻返回。
func (b *Bus) Publish(ctx context.Context, ev Event) error {
	select {
	case b.ch <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Drain 等"此刻之前发出去的便签"全部被处理完。退出前调一次。
//
// 做法：往同一条 channel 塞一张空白便签当挡板（Type 为空）。
// Run 是单 goroutine FIFO 的，挡板一定排在之前所有便签后面，
// 所以它被处理到的那一刻，前面的必然都处理完了。
//
// 【必记】挡板管"顺序"，ack 管"我知道它到了"，两个缺一不可：
// b.ch <- Event{} 发送成功只代表挡板进了缓冲区，不代表 Run 取走了它。
// 没有 ack，Drain 塞完就返回，等于什么都没等。
//
// 限制：用字段存 ack，所以同一时刻只能有一个 Drain 在等。
// 只在退出前调一次，不要并发调用。
func (b *Bus) Drain(ctx context.Context) error {
	ack := make(chan struct{})
	b.mu.Lock()
	b.ack = ack
	b.mu.Unlock()

	select {
	case b.ch <- Event{}: // Type 为空 = 挡板
	case <-ctx.Done():
		// 挡板没塞进去，把回执撤掉，免得下次 Drain 拿到个永远不会关的 channel。
		b.mu.Lock()
		b.ack = nil
		b.mu.Unlock()
		return ctx.Err()
	}

	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run 开始分发。单 goroutine，一直跑到 ctx 取消。
//
// 【必记】单 goroutine 换来的是一件事：顺序。
// 事件 1、2、3 进来，处理顺序一定是 1、2、3。
// 代价是任何 agent 处理慢了，后面全堵——
// 所以慢活（调模型）不许在 Handle 里同步做，
// 要自己扔进内部队列后立刻返回。
func (b *Bus) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-b.ch:
			b.dispatch(ctx, ev)
		}
	}
}

// dispatch 把一张便签分给所有关心它的人。
func (b *Bus) dispatch(ctx context.Context, ev Event) {
	// 挡板：不是真事件，只用来告诉 Drain "排在我前面的都处理完了"。
	// 它靠的是 Run 单 goroutine FIFO 这个性质，所以千万别把 Run 改成并发分发。
	if ev.Type == "" {
		b.mu.Lock()
		if b.ack != nil {
			close(b.ack)
			b.ack = nil
		}
		b.mu.Unlock()
		return
	}

	b.mu.RLock()
	agents := b.subs[ev.Type]
	b.mu.RUnlock()

	if len(agents) == 0 {
		// 没人听的事件，八成是事件名写错了。宁可多一行日志。
		// 这类 bug 不会崩，只会"某个功能莫名其妙不生效"。
		log.Printf("总线: %s 没有订阅者（session=%s idx=%d）", ev.Type, ev.SessionID, ev.Idx)
		return
	}

	for _, a := range agents {
		// 一个 agent 报错不能带走后面的：这条是隔离，
		// 打分器挂了不该让报告也收不到事件。
		if err := a.Handle(ctx, ev); err != nil {
			log.Printf("总线: %s 处理 %s 失败: %v", a.Name(), ev.Type, err)
		}
	}
}
