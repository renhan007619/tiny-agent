package interview

import (
	"fmt"
	"regexp"
	"strings"
)

// Tier 是离散档位。
//
// 为什么用字母不用 0-10 的数字：
// 数字有 11 个可选值，模型会挑中间那个（"8 题全挤在 4~6 分"就是这么来的）。
// 字母只有 5 个、每档都有名字，模型做的是"这段话像哪一档"的匹配，
// 而不是"该给几分"的评估——后者的自由度高到它一定会飘。
type Tier string

const (
	TierA Tier = "A" // 讲清了原理、边界、取舍，有数据
	TierB Tier = "B" // 讲清了怎么实现，取舍或数据缺一样
	TierC Tier = "C" // 主干说对了，具体细节基本空着
	TierD Tier = "D" // 只是把名词报了一遍，没说清怎么做
	TierE Tier = "E" // 说法明显错误，或者答非所问、基本没答
)

// tierOrder 从好到差。命中扣分项就往右挪，顺序不能乱。
var tierOrder = []Tier{TierA, TierB, TierC, TierD, TierE}

var tierScore = map[Tier]int{TierA: 10, TierB: 8, TierC: 6, TierD: 4, TierE: 2}

var tierDesc = map[Tier]string{
	TierA: "讲清了原理、边界和取舍，有数据或具体例子支撑",
	TierB: "讲清了怎么实现，但取舍或数据缺一样",
	TierC: "主干说对了，具体细节基本空着",
	TierD: "只是把名词报了一遍，没说清具体怎么做",
	TierE: "说法明显错误，或者答非所问、基本没答",
}

// Score 把档位换成 0-10 的分。报告里的平均分和以后的复习排期都用它。
func (t Tier) Score() int {
	if s, ok := tierScore[t]; ok {
		return s
	}
	return 0
}

// Index 是档位在「从好到差」队列里的位置：A=0，E=4。
func (t Tier) Index() int {
	for i, v := range tierOrder {
		if v == t {
			return i
		}
	}
	return len(tierOrder) - 1
}

// Desc 是这一档的说明，拼提示词和写报告都用它。
func (t Tier) Desc() string {
	if d, ok := tierDesc[t]; ok {
		return d
	}
	return "未知档位"
}

// Valid 判断是不是合法的档位。
func (t Tier) Valid() bool {
	_, ok := tierScore[t]
	return ok
}

// ParseTier 把模型给的一个字母读成档位。
// 大小写、前后空格都容忍；给了别的东西就判为不合法，让上层去兜。
func ParseTier(s string) (Tier, bool) {
	t := Tier(strings.ToUpper(strings.TrimSpace(s)))
	if t.Valid() {
		return t, true
	}
	return "", false
}

// TierFromScore 把 0-10 的分反推成档位。
//
// 只给「改造前存进库里的老数据」用——老记录只有分数没有档位，
// 读出来时换算一下，报告里才不会一半有档位一半没有。
// 新数据一律是 CombineTier 算出来的，不走这条路。
func TierFromScore(total int) Tier {
	switch {
	case total >= 9:
		return TierA
	case total >= 7:
		return TierB
	case total >= 6:
		return TierC
	case total >= 4:
		return TierD
	default:
		return TierE
	}
}

// DeductionKind 说明这一项由谁来判。
type DeductionKind string

const (
	// DeductByCode 代码就能判（数数字、数字数）。零成本、结果永远一样。
	DeductByCode DeductionKind = "code"
	// DeductByJudge 得读懂内容才能判，交给模型。
	DeductByJudge DeductionKind = "judge"
)

// Deduction 是一个扣分项。
//
// ★ 最重要的一条设计规矩 ★
//
//	Hint 只描述「缺了什么关注点」，绝不描述「必须提到的具体答案」。
//
// 反例（错）："没有提到用 exec.CommandContext 杀子进程"
//
//	→ 这是背答案。候选人用 cmd.Process.Kill() 达到同样效果也会被误判。
//
// 正例（对）："没有说明取消后如何终止底层执行"
//
//	→ 只看有没有覆盖这个关注点，不管用什么手段。
//
// 一旦写成反例那样，判卷就退化成"和标准答案做字符串匹配"，
// 而面试回答本来就有很多种正确讲法。
type Deduction struct {
	ID   string        // 稳定 id，统计和数据标注都靠它，不许随便改
	Name string        // 中文名，报告里显示
	Kind DeductionKind // code = 代码判，judge = 模型判
	Down int           // 命中后档位往差里挪几格
	Hint string        // 给模型看的判断标准
}

// deductions 是全部扣分项。
//
// 只有 7 条，是有意的：条目越多模型越爱乱扣，报告也没法读。
// 排序也有讲究——重的在前，模型读到的第一条就是最狠的那条。
var deductions = []Deduction{
	{
		ID:   "wrong_core",
		Name: "核心概念说错",
		Kind: DeductByJudge,
		Down: 4, // 一步到 E
		Hint: "把核心概念、机制或结论说错了，错到会影响技术判断。只是措辞不严谨、或者用了别的说法但意思对，不算。",
	},
	{
		ID:   "off_topic",
		Name: "答非所问",
		Kind: DeductByJudge,
		Down: 3,
		Hint: "通篇没有回答被问的那个问题：要么整段是通用套话（什么都要看场景、没有银弹、看团队能力），要么只是把题目复述了一遍。注意：只要有一段在正面回答，就不算这一项。",
	},
	{
		ID:   "name_only",
		Name: "只报名词没讲实现",
		Kind: DeductByJudge,
		Down: 2,
		Hint: "通篇只是把技术名词报了一遍，没有讲这个名词在他这里具体怎么用、解决什么问题、怎么实现的。",
	},
	{
		ID:   "no_tradeoff",
		Name: "没讲取舍",
		Kind: DeductByJudge,
		Down: 1,
		Hint: "只说了选了什么做法，没有说为什么这么做、不这么做会怎样。注意：讲了具体理由才算，「看情况」「具体分析」这种空话不算理由。",
	},
	{
		ID:   "no_boundary",
		Name: "没提边界或异常",
		Kind: DeductByJudge,
		Down: 1,
		Hint: "没有提到任何边界情况、异常情况或失败场景（比如超时、为空、并发冲突、出错了怎么办、极端输入）。",
	},
	{
		ID:   "no_number",
		Name: "没有量化",
		Kind: DeductByCode, // 代码数数字，不花 token
		Down: 1,
		Hint: "通篇没有任何数字、量级或比例（比如几万 QPS、省了 30%、2 秒、八个并发）。",
	},
	{
		ID:   "too_short",
		Name: "回答过短",
		Kind: DeductByCode,
		Down: 4, // 一步到 E
		Hint: "回答不到 20 个字，实际上没有回答问题。",
	},
}

// deductionByID 查表用。
var deductionByID = func() map[string]Deduction {
	m := make(map[string]Deduction, len(deductions))
	for _, d := range deductions {
		m[d.ID] = d
	}
	return m
}()

// Deductions 返回全部扣分项（保持定义顺序）。
func Deductions() []Deduction { return deductions }

// DeductionByID 按 id 查一条。
func DeductionByID(id string) (Deduction, bool) {
	d, ok := deductionByID[id]
	return d, ok
}

// DeductionName 把 id 换成人话；不认识的 id 原样返回，免得悄悄吃掉。
func DeductionName(id string) string {
	if d, ok := deductionByID[id]; ok {
		return d.Name
	}
	return id
}

// DeductionPromptList 把扣分项拼成给模型看的清单。
//
// 只列 judge 类：code 类由代码自己数，写进提示词只会让模型重复扣一遍。
func DeductionPromptList() string {
	var sb strings.Builder
	for _, d := range deductions {
		if d.Kind != DeductByJudge {
			continue
		}
		fmt.Fprintf(&sb, "- %s（%s）：%s\n", d.ID, d.Name, d.Hint)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// 代码判卷用的两个工具。
var numberRe = regexp.MustCompile(`\d`)

// 除了阿拉伯数字，这些词也算"给了量级"。
// 故意不含「次」「条」「个」这类量词：说"收了三次"不等于给了任何量化信息。
var scaleWords = []string{"倍", "万", "千", "百", "%", "ms", "毫秒", "秒", "分钟",
	"GB", "MB", "KB", "QPS", "TPS"}

// minAnswerRunes 是「回答过短」的线。
// 用「字」数不是字节数：中文一个字三字节，按字节算会把二十个字算成七个。
const minAnswerRunes = 20

// CheckByCode 跑代码判卷，返回命中的扣分项 id。
//
// 这一层不花 token、结果永远一样，专门兜住模型最容易漏的两项：
// 字数太少、通篇没有数字。
// 模型看的是「讲得对不对」，它很容易忽略「一个数字都没有」这种整体特征——那就交给代码数。
func CheckByCode(answer string) []string {
	var hits []string
	s := strings.TrimSpace(answer)

	if len([]rune(s)) < minAnswerRunes {
		hits = append(hits, "too_short")
	}

	if !numberRe.MatchString(s) {
		found := false
		for _, w := range scaleWords {
			if strings.Contains(s, w) {
				found = true
				break
			}
		}
		if !found {
			hits = append(hits, "no_number")
		}
	}
	return hits
}

// MergeHits 把几批命中的扣分项合起来，去掉重复的，保持稳定顺序。
//
// 顺序必须稳定：同样的输入要得到同样的档位。不然同一段回答跑两次
// 分数不一样，这个工具就没法用来判断自己有没有进步。
func MergeHits(groups ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, g := range groups {
		for _, id := range g {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// CombineTier 把命中的扣分项折算成档位。
//
// 从满分档 A 开始，命中一项就往差里挪 Down 格，挪到头就是 E。
//
// 这就是「扣分制」落到代码上的样子：模型只报「踩了哪几条」，
// 分数由代码算。好处有两个——
//  1. 同一段回答跑两次，只要命中的项一样，分数一定一样（可复现）
//  2. 模型没有「我觉得还不够完美，扣两分留点余地」的权限，天花板才是开着的
func CombineTier(hits []string) Tier {
	idx := 0
	for _, id := range MergeHits(hits) {
		d, ok := deductionByID[id]
		if !ok {
			continue // 不认识的 id 忽略，不让它把分数拉偏
		}
		idx += d.Down
	}
	if idx >= len(tierOrder) {
		idx = len(tierOrder) - 1
	}
	return tierOrder[idx]
}
