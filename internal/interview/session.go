package interview

import (
	"fmt"
	"time"
)

// 一场面试分五个阶段，按顺序走，走完为止。
type Stage int

const (
	StageGreeting  Stage = iota // 开场：打个招呼
	StageSelfIntro              // 自我介绍
	StageTechnical              // 技术问题
	StageProject                // 挖简历里的项目
	StageReverseQA              // 反问环节：让候选人提问
)

// 每个阶段问够几题就往下走。
var stagePlan = []struct {
	Name  string
	Quota int
}{
	{"开场", 1},
	{"自我介绍", 1},
	{"技术基础", 3},
	{"项目深挖", 2},
	{"反问环节", 1},
}

// MaxQuestions 是全场硬上限。不管表怎么写，也越不过这条线。
const MaxQuestions = 10

// Name 是阶段的中文名，打印用。
func (st Stage) Name() string {
	if int(st) >= 0 && int(st) < len(stagePlan) {
		return stagePlan[st].Name
	}
	return "结束"
}

// GradeState 是"打分这件事走到哪一步了"。
//
// 为什么要有这一列状态，而不是看 Score 是不是 nil：
// 因为"还没轮到""算了失败""算出来了"这三件事，在 nil 上长得一模一样。
type GradeState string

const (
	GradePending     GradeState = "pending"     // 还没轮到
	GradeRunning     GradeState = "running"     // 正在算
	GradeDone        GradeState = "done"        // 算出结果了
	GradeSkipped     GradeState = "skipped"     // 这一题不计分（反问环节）
	GradeFailed      GradeState = "failed"      // 被打断（比如进程要退），可以重跑
	GradeUnavailable GradeState = "unavailable" // 重试用尽，判为拿不到
)

// Turn 是一问一答。
type Turn struct {
	Index    int        // 第几题，从 1 开始，也是数据库主键的一部分
	Stage    Stage      // 这题属于哪个阶段
	Question string     // 面试官说的话
	Answer   string     // 候选人的回答
	Grade    GradeState // 打分走到哪一步了
	Score    *Score     // 只有 Grade == GradeDone 时才可能有值
	Review   *Review    // 同上
	GradeErr string     // 失败原因，报告里要显示给人看
}

// Session 就是一场正在进行的面试。
//
// 它只有一个字段是真数据：Turns。
// 现在到第几阶段、已经问了几题，都不存，每次现算。
type Session struct {
	ID        string
	StartedAt time.Time
	Turns     []Turn

	// Focus 是本场重点考点，开场挑一次，之后不再变。
	//
	// 为什么"不再变"很重要：每轮 Prompt 都会重新拼一遍，
	// 如果这内容每轮都动，提示词前缀就一直在变，缓存全作废。
	Focus []FocusPoint
}

// NewSession 开一场新面试。
func NewSession(id string, at time.Time) *Session {
	return &Session{ID: id, StartedAt: at}
}

// stageOf 看历史一共问了哪些题，算出"现在在第几阶段、这个阶段问了几题"。
//
// 这是全文件最重要的函数：它是纯函数，同样的 Turns 进去永远是同样的结果，
// 它不修改任何东西。所以不存在"阶段字段和题目对不上"这种事。
func stageOf(turns []Turn) (Stage, int) {
	st, asked := StageGreeting, 0
	for range turns {
		// 当前阶段问满了就往前挪。可能一次挪好几格（比如配额被改小了）。
		for int(st) < len(stagePlan) && asked >= stagePlan[st].Quota {
			st++
			asked = 0
		}
		if int(st) >= len(stagePlan) {
			return st, 0 // 所有阶段都走完了
		}
		asked++
	}
	// 最后一题问完也得判断一次，不然要等下一题才知道该换阶段。
	for int(st) < len(stagePlan) && asked >= stagePlan[st].Quota {
		st++
		asked = 0
	}
	return st, asked
}

// CurrentStage 返回当前阶段和这个阶段已经问了几题。
func (s *Session) CurrentStage() (Stage, int) { return stageOf(s.Turns) }

// Total 返回全场一共问了几题。
func (s *Session) Total() int { return len(s.Turns) }

// Over 判断面试结束了没有。
func (s *Session) Over() bool {
	if len(s.Turns) >= MaxQuestions {
		return true
	}
	st, _ := stageOf(s.Turns)
	return int(st) >= len(stagePlan)
}

// nextQuestionRule 是"先看上一题答得怎么样，再决定这道问什么"的规矩。
//
// 为什么不问打分器要分数，而是让面试官自己判：
//  1. 它本来就在读候选人的回答，比任何人都有资格判断他听懂没有
//  2. 找打分器要分数就得等 —— 打分是异步的，等它对话就卡住了
//
// 这个功能上线前，题目之间是彼此独立的：你答崩了也照样按计划往下问。
// 注意：这里没有"追问"的代码。判断和决策都发生在模型那边，
// 代码只负责把规矩说清楚 —— 这正是 agent 和 workflow 的分界线。
const nextQuestionRule = `
出下一题之前，先在心里判断他刚才那段回答到哪一层（这个判断不要写出来）：

  第1层  只报了名词或结论，比如"channel 是通信用的""sync 是加锁的"，
         但没解释它到底怎么工作
  第2层  说清了怎么工作：讲了机制、给了用法，但没提代价
  第3层  讲到了代价和边界：说了什么情况下会出问题、为什么不用另一种做法

然后按判断决定这一题问什么：

  第1层  追问同一个点，但换一个更具体的角度（给个场景让他代入）。
         不许原样再问一遍，也不许直接换话题
  第2层  顺着这个点往下挖一层：问代价、问边界、问"那如果……"。
         这一次他要是答到了第3层，下一个问题就换话题
  第3层  直接换话题，别在这个点上继续耗

几条硬限制：
  - 不许降低难度。追问不出来就换下一个考点（难度保持不变），
    不要改成"更基础的问题"，也不要"给点提示再问个简单的"——
    这一场是在量他到底有多深，尺子不能变
  - 同一个点最多追问一次。追过一次还答不出来就换话题 ——
    面试要覆盖广度，不能在一个坑里耗光时间
  - 你说的每一句话都只能是题目本身，不要写"你刚才提到了X""那我问问"
    这类过渡语，也不要说明你为什么这么问
  - 不要说分数，也不要说你判断他到了第几层
`

// Prompt 每轮重新拼一次，告诉模型"现在到哪了、该问什么"。
func (s *Session) Prompt() string {
	if s.Over() {
		return "面试已经结束。请跟候选人道别，不要再出题。"
	}

	st, asked := stageOf(s.Turns)
	plan := stagePlan[st]
	left := plan.Quota - asked

	// 反问环节是唯一一个"你出题、我答"反转的阶段，单独说清楚。
	if st == StageReverseQA {
		return fmt.Sprintf(
			"现在进入反问环节：让候选人向你提问，你来回答。\n"+
				"不要自己出题。回答完如果他还想追问就继续，他说没问题了就道别。\n"+
				"（第 %d 阶段，共 %d 阶段）",
			int(st)+1, len(stagePlan),
		)
	}

	// 和以前比改了两处：
	//   1. 去掉"先用一句话点评" —— 点评会暴露判断，面试就不真了
	//   2. 末尾接一段"先判断再出题"的规矩
	base := fmt.Sprintf(
		"你是一名严格但公正的校招面试官，方向是 Go 后端。\n"+
			"当前阶段：%s（第 %d 阶段，共 %d 阶段），这个阶段还要问 %d 个问题。\n"+
			"规矩：一次只问一个问题；不许替候选人回答；不要说分数、不要提评分标准。",
		plan.Name, int(st)+1, len(stagePlan), left,
	)

	// 第一题还没有"上一题"可判断，所以这段只在答过题之后才贴。
	// 不贴的话模型会对着空历史硬判，可能一上来就追问。
	if len(s.Turns) > 0 && s.Turns[len(s.Turns)-1].Answer != "" {
		base += "\n\n" + nextQuestionRule
	}

	// 重点只贴在"我出题"的阶段。反问环节是候选人提问，
	// 那时考官不该带着"要考他什么"的心思，所以上面那个分支提前返回、走不到这儿。
	if brief := FocusBrief(s.Focus); brief != "" {
		return base + "\n" + brief
	}
	return base
}

// AddQuestion 记下模型刚问出的那句话。
func (s *Session) AddQuestion(q string) {
	if s.Over() {
		return
	}
	st, _ := stageOf(s.Turns)
	s.Turns = append(s.Turns, Turn{
		Index:    len(s.Turns) + 1,
		Stage:    st,
		Question: q,
		Grade:    GradePending,
	})
}

// Answer 记下候选人的回答。
//
// 注意这里不再"推进阶段"：阶段跟着 Turns 走，不用手动改。
func (s *Session) Answer(a string) {
	if len(s.Turns) == 0 {
		return
	}
	s.Turns[len(s.Turns)-1].Answer = a
}

// History 把记录转成发给模型的对话历史。
func (s *Session) History() []Message {
	var out []Message
	for _, t := range s.Turns {
		out = append(out, Message{Role: RoleAssistant, Text: t.Question})
		if t.Answer != "" {
			out = append(out, Message{Role: RoleUser, Text: t.Answer})
		}
	}
	return out
}

// StageQuota 是"某个阶段该问几题"的只读视图。
//
// 为什么要有它：stagePlan 是包内私有变量，外部（比如轨迹体检）想按配额
// 判违规却拿不到它。开一个只读窗口，比把 stagePlan 整个导出安全——
// 导出变量谁都能改，导出函数只能读。
type StageQuota struct {
	Stage Stage
	Name  string
	Quota int
}

// StageQuotas 返回全部阶段的配额，按顺序。
//
// 返回的是新切片：调用方 append 或者改元素，都碰不到 stagePlan 本身。
func StageQuotas() []StageQuota {
	out := make([]StageQuota, len(stagePlan))
	for i, p := range stagePlan {
		out[i] = StageQuota{Stage: Stage(i), Name: p.Name, Quota: p.Quota}
	}
	return out
}
