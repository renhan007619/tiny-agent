package interview

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeAgent 是测试用的假 agent：收到事件就塞进 channel。
type fakeAgent struct {
	name    string
	handles []EventType
	got     chan Event
	failOn  EventType // 收到这类事件就故意报错
}

func newFakeAgent(name string, buf int, handles ...EventType) *fakeAgent {
	return &fakeAgent{name: name, handles: handles, got: make(chan Event, buf)}
}

func (f *fakeAgent) Name() string         { return f.name }
func (f *fakeAgent) Handles() []EventType { return f.handles }

func (f *fakeAgent) Handle(_ context.Context, ev Event) error {
	f.got <- ev
	if ev.Type == f.failOn {
		return errors.New("故意失败")
	}
	return nil
}

// startBus 起一个总线，返回它的 ctx 取消函数。
func startBus(t *testing.T) (*Bus, context.CancelFunc) {
	t.Helper()
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	t.Cleanup(cancel)
	return b, cancel
}

// 贴一张便签，关心它的人就得收到。
func TestBus_Delivers(t *testing.T) {
	b, _ := startBus(t)
	a := newFakeAgent("打分器", 1, EvQuestionAnswered)
	b.Subscribe(a)

	if err := b.Publish(context.Background(), Event{
		Type: EvQuestionAnswered, SessionID: "s1", Idx: 3,
	}); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case got := <-a.got:
		if got.Idx != 3 || got.SessionID != "s1" {
			t.Fatalf("事件内容被改了: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("等了 1 秒没收到事件")
	}
}

// 多个 agent 关心同一类事件，都要收到——这就是"一个事件，多个消费者"。
func TestBus_MultipleSubscribers(t *testing.T) {
	b, _ := startBus(t)
	a := newFakeAgent("A", 1, EvGradeDone)
	c := newFakeAgent("B", 1, EvGradeDone)
	b.Subscribe(a)
	b.Subscribe(c)

	_ = b.Publish(context.Background(), Event{Type: EvGradeDone})

	for _, f := range []*fakeAgent{a, c} {
		select {
		case <-f.got:
		case <-time.After(time.Second):
			t.Fatalf("%s 没收到事件", f.name)
		}
	}
}

// 只关心自己那类，别的不该收到。
func TestBus_OnlyItsOwnTypes(t *testing.T) {
	b, _ := startBus(t)
	a := newFakeAgent("只关心打分", 4, EvGradeDone)
	b.Subscribe(a)

	_ = b.Publish(context.Background(), Event{Type: EvQuestionAsked})

	select {
	case ev := <-a.got:
		t.Fatalf("不该收到 %s", ev.Type)
	case <-time.After(100 * time.Millisecond):
		// 100ms 没收到就算对——这里必须等，因为"没发生"没法同步通知
	}
}

// 【必记】顺序：连发三个，收到必须是 1、2、3。
// 这条守住"单 goroutine 分发"这个设计，改成并发分发它立刻红。
func TestBus_KeepsOrder(t *testing.T) {
	b, _ := startBus(t)
	a := newFakeAgent("收", 3, EvQuestionAnswered)
	b.Subscribe(a)

	for i := 1; i <= 3; i++ {
		_ = b.Publish(context.Background(), Event{Type: EvQuestionAnswered, Idx: i})
	}

	for want := 1; want <= 3; want++ {
		select {
		case got := <-a.got:
			if got.Idx != want {
				t.Fatalf("顺序乱了：想要第 %d 个，收到第 %d 个", want, got.Idx)
			}
		case <-time.After(time.Second):
			t.Fatalf("第 %d 个没收到", want)
		}
	}
}

// 一个 agent 处理失败，不能带走后面的。
func TestBus_ErrorIsolation(t *testing.T) {
	b, _ := startBus(t)
	bad := newFakeAgent("会挂的", 1, EvGradeDone)
	bad.failOn = EvGradeDone
	good := newFakeAgent("正常的", 1, EvGradeDone)
	b.Subscribe(bad)
	b.Subscribe(good)

	_ = b.Publish(context.Background(), Event{Type: EvGradeDone})

	select {
	case <-good.got:
	case <-time.After(time.Second):
		t.Fatal("前面的 agent 挂了，后面的不该跟着收不到")
	}
}

// 同一类事件重复订阅，不能处理两次。
func TestBus_NoDuplicateDelivery(t *testing.T) {
	b, _ := startBus(t)
	a := newFakeAgent("重复订阅", 4, EvGradeDone)
	b.Subscribe(a)
	b.Subscribe(a) // 又来一次

	_ = b.Publish(context.Background(), Event{Type: EvGradeDone})

	select {
	case <-a.got:
	case <-time.After(time.Second):
		t.Fatal("应该收到一次")
	}
	select {
	case <-a.got:
		t.Fatal("同一条事件被处理了两次")
	case <-time.After(100 * time.Millisecond):
	}
}

// 没人关心的事件，不能崩。这条只是防止 panic。
func TestBus_NoSubscriberIsSafe(t *testing.T) {
	b, _ := startBus(t)
	if err := b.Publish(context.Background(), Event{Type: EvSessionFinished}); err != nil {
		t.Fatalf("没订阅者也不该报错: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // 给分发 goroutine 一点时间去打那行日志
}

// 【必记】公告栏满了 + 调用方已取消，Publish 必须立刻返回，不能永久卡住。
func TestBus_PublishUnblocksOnCancel(t *testing.T) {
	b := NewBus() // 故意不启动 Run：没人消费，channel 迟早满

	ctx := context.Background()
	for i := 0; i < busBufferSize; i++ {
		if err := b.Publish(ctx, Event{Type: EvQuestionAnswered, Idx: i}); err != nil {
			t.Fatalf("填缓冲失败: %v", err)
		}
	}

	// 现在缓冲满了，再发就会等
	cctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	if err := b.Publish(cctx, Event{Type: EvQuestionAnswered}); !errors.Is(err, context.Canceled) {
		t.Fatalf("应该返回 context.Canceled，实际: %v", err)
	}
}

// Run 收到取消信号要退出，不能泄漏 goroutine。
func TestBus_RunStops(t *testing.T) {
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 没退出，goroutine 泄漏了")
	}
}
