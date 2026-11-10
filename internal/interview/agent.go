package interview

import "context"

// Agent 是看公告的人：统一长相，只认这一个接口。
//
// 三个方法各有分工：
//
//	Name    —— 日志里得知道是谁出的错
//	Handles —— 我关心哪几类事件（注册时用，不用把六个都接）
//	Handle  —— 收到一张便签，干我自己的活
//
// 【必记】这里没有 Init/Destroy/Start/Stop 那一套。
// 需要长期跑东西的（比如打分队列），自己
// 在自己的构造函数里起，别把这个接口撑成生命周期框架。
type Agent interface {
	Name() string
	Handles() []EventType
	Handle(ctx context.Context, ev Event) error
}
