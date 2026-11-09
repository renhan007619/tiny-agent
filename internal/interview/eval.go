// Package interview 是模拟面试官的业务代码。
//
// 这里只有一条规矩：聊天归聊天，打分归打分。
// 聊天那条路上，模型只说人话，一个字的结构都不吐——用户等着看下一题，等不起。
// 打分在后台单独跑，这一题答完了就开始算，算多久都没人等。
package interview

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 打分拆成两块，各管各的：
//
//	Score  只管"踩了哪几条坑"——用来算档位、算平均分、跨场次对比
//	Review 只管"这次具体差在哪"——用人话写，给人看
//
// 为什么分数不写进 Score：
// 因为分数现在不由模型给，由代码用 CombineTier 算出来（见 rubric.go）。
// 模型报 hits，代码算档位——模型没有"我觉得扣两分比较合适"这个权限。

// Score 是一次判卷的结论。
//
// 三个字段里只有一个来自模型：
//
//	Hits     模型报的「踩了哪几条坑」  ← 模型给的
//	Tier     代码用 CombineTier 算的档位   ← 算出来的
//	Total    代码用档位查出来的分         ← 算出来的
//
// 以前这里是 total/depth/project/talk/solve 五个数字，全由模型自由填写。
// 那套东西的问题是：它给了模型一个 0~10 的自由赋值空间，它就在中间挑。
// 8 道题全挤在 4~6 分，不是因为它懒，是因为"给几分"这个问题本身太开放。
type Score struct {
	Tier     Tier              `json:"-"`        // A-E，代码算的
	Hits     []string          `json:"hits"`     // 命中的扣分项 id
	Evidence map[string]string `json:"evidence"` // 每项一句原文依据，可以空
	Total    int               `json:"-"`        // = Tier.Score()
	Partial  bool              `json:"-"`        // 这些数字是补出来的，不是模型原话

	// 下面两个是 L2 事件层：描述"讲到多深"，不参与算分。
	//
	// 为什么要它们：原文读不动了。30 题的原文模型读不完，
	// 但「哪几题永远只停在第一层」这种 30 行事实读得下。
	Depth   int    `json:"depth"`   // 0~3，讲到第几层
	Suspect string `json:"suspect"` // 可疑点，空表示没有
}

// WeakPoint 一条弱点。
//
// 为什么拆成两半，而不是像以前那样就一句话：
//
//	Tag    管"是哪个毛病" —— 用来数次数、跨场次对比，同一个毛病必须永远同一个名字
//	Detail 管"这次具体怎么犯的" —— 用来看细节，可以自由写
//
// 只留一句话的话，第二次说同一个毛病时措辞一变，就数不上次数了
// （这正是"32 条毛病全是 1 次"那个毛病的根）。
type WeakPoint struct {
	Tag    string `json:"tag"`    // 必须是标签库里的名字，由 Resolve 兜底
	Detail string `json:"detail"` // 这次具体差在哪
}

// Review 只装文字。
type Review struct {
	Comment    string      `json:"comment"`     // 一句话评语
	WeakPoints []WeakPoint `json:"weak_points"` // 差在哪，一条一个

	// 含义同 Score.Partial。
	Partial bool `json:"-"`
}

// 模型输出里，用这两对标记把两块内容框起来。
//
// 为什么不用 <!-- --> 这种注释来框？
// 因为模型回答里如果出现 --> 这几个字，那一行会被从中间切断，后面的内容全丢。
const (
	JudgeOpen   = "<<<JUDGE"
	JudgeClose  = "JUDGE>>>"
	ReviewOpen  = "<<<REVIEW"
	ReviewClose = "REVIEW>>>"
)

// 失败分两种，处理方式不一样。
var (
	// ErrNoGrade：模型压根没给打分内容。这一题就没分，标成待补。
	ErrNoGrade = errors.New("模型没给打分内容")
	// ErrBadJSON：内容找到了，但里面写坏了，修也修不通。
	ErrBadJSON = errors.New("打分内容读不出来")
)

// ParseGrade 把模型的一次判卷回答拆成两块。
//
//	s —— 判卷结论，为 nil 表示这块没拿到
//	r —— 文字评语，为 nil 表示这块没拿到
//
// 两块可能单独缺：只给了 hits 没给评语，也算成功，只是 r 为 nil。
// 两块都没有才报 ErrNoGrade。
func ParseGrade(raw string) (s *Score, r *Review, err error) {
	judgePayload, hasJudge := takeBlock(raw, JudgeOpen, JudgeClose)
	reviewPayload, hasReview := takeBlock(raw, ReviewOpen, ReviewClose)

	if !hasJudge && !hasReview {
		return nil, nil, ErrNoGrade
	}

	// 两块各自独立处理，一块坏了不影响另一块。
	var firstErr error

	if hasJudge {
		var sc Score
		repaired, e := unmarshalLoose(judgePayload, &sc)
		if e != nil {
			firstErr = e
		} else {
			sc.Partial = repaired
			sc.normalize()
			s = &sc
		}
	}

	if hasReview {
		var rv Review
		repaired, e := unmarshalLoose(reviewPayload, &rv)
		if e != nil {
			if firstErr == nil {
				firstErr = e
			}
		} else {
			rv.Partial = repaired
			rv.normalize()
			r = &rv
		}
	}

	return s, r, firstErr
}

// takeBlock 从一段话里，把这对标记框住的内容拿出来。
//
// 找最后一对、不是第一对：我们给模型的示例里也可能有这对标记，
// 模型有时候会把示例一起抄出来，前面那个是假的，最后面那个才是真结果。
func takeBlock(raw, open, closeMark string) (string, bool) {
	closeIdx := strings.LastIndex(raw, closeMark)
	if closeIdx < 0 {
		return "", false
	}
	openIdx := strings.LastIndex(raw[:closeIdx], open)
	if openIdx < 0 {
		return "", false
	}
	return strings.TrimSpace(raw[openIdx+len(open) : closeIdx]), true
}

// unmarshalLoose 把一段文字读进 Go 的结构。
// 直接读不通，就简单修一下再读一次。
// 返回值 repaired 为 true 表示"修过才读通"，调用方要给数据打记号。
func unmarshalLoose(payload string, v any) (repaired bool, err error) {
	if err := json.Unmarshal([]byte(payload), v); err == nil {
		return false, nil
	}
	if err := json.Unmarshal([]byte(repairJSON(payload)), v); err != nil {
		return true, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	return true, nil
}

var trailingCommaRe = regexp.MustCompile(`,(\s*[}\]])`)

// repairJSON 只修三种常见的坏写法，修不好就原样还回去，让上面报错。
//
// 为什么不写一个"什么都能修"的版本：
// 修错了它不会报错，而是安安静静给你一份错数据，拿这份错数据去做判断
// 比直接报错危险得多。
func repairJSON(s string) string {
	s = strings.TrimSpace(s)
	// 坏法一：模型把整段包进了代码块
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```JSON")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// 坏法二：多了一个逗号，比如 {"a": 1,}
	s = trailingCommaRe.ReplaceAllString(s, "$1")
	// 坏法三：说太长了被切断，右半边少了括号，补上
	for strings.Count(s, "{") > strings.Count(s, "}") {
		s += "}"
	}
	return s
}

// normalize 把模型报的 hits 整理成能直接用的样子。
//
// 做四件事，顺序不能换：
//  1. 去重（模型偶尔会把同一项写两遍）
//  2. 丢掉不认识的 id，但打 Partial 记号——丢掉是防它把分拉偏，记下来是防我们不知道它胡说
//  3. 用清理后的 hits 算档位
//  4. 用档位算分
func (s *Score) normalize() {
	valid := make([]string, 0, len(s.Hits))
	for _, id := range MergeHits(s.Hits) {
		if _, ok := DeductionByID(id); ok {
			valid = append(valid, id)
		} else {
			s.Partial = true
		}
	}
	s.Hits = valid
	if s.Hits == nil {
		// 空列表不能留成 nil：JSON 化会变成 null，前端拿去遍历就崩
		s.Hits = []string{}
	}

	s.Tier = CombineTier(s.Hits)
	s.Total = s.Tier.Score()

	// evidence 只保留还在 hits 里的那些键，免得两边对不上（显示时找不到对应条目）
	if len(s.Evidence) > 0 {
		keep := make(map[string]string, len(s.Hits))
		inHits := make(map[string]bool, len(s.Hits))
		for _, id := range s.Hits {
			inHits[id] = true
		}
		for id, v := range s.Evidence {
			v = strings.TrimSpace(v)
			if v == "" || !inHits[id] {
				continue
			}
			keep[id] = v
		}
		s.Evidence = keep
	}

	// —— L2：层数和可疑点 ——
	//
	// 故意写在这里、而不是参与上面算分：
	// 档位只认 hits，depth 再怎么变都不该动分数，
	// 否则同一段回答跑两次档位会不一样，确定性就没了。
	//
	// 夹到 0~3：模型偶尔写 4、写 -1，或者写个"3层"解析失败退成 0。
	// 越界不报错、静默夹回来——它只是个计量，不值得为它中断整题。
	if s.Depth < 0 {
		s.Depth = 0
	}
	if s.Depth > 3 {
		s.Depth = 3
	}
	s.Suspect = strings.TrimSpace(s.Suspect)
}

// normalize 把文字整理干净。
func (r *Review) normalize() {
	r.Comment = strings.TrimSpace(r.Comment)
	// 弱点列表不能留空指针。它会变成 null，
	// 前端拿到 null 再去一条条读，直接就崩了。
	if r.WeakPoints == nil {
		r.WeakPoints = []WeakPoint{}
	}
	for i := range r.WeakPoints {
		r.WeakPoints[i].Tag = strings.TrimSpace(r.WeakPoints[i].Tag)
		r.WeakPoints[i].Detail = strings.TrimSpace(r.WeakPoints[i].Detail)
	}
}

// 协议模板里两个要运行时填进去的位置。
const (
	gradeTagSlot    = "<可用标签>"
	gradeDeductSlot = "<可用扣分项>"
)

// gradeProtocolTemplate 是面试结束后，单独发给模型的那段话：
// 让它对着刚才那一段问答，判断这份回答踩了哪些坑。
//
// 注意：这段话不会出现在聊天里。聊天那条路上模型什么都不用填。
//
// 硬规矩第 1 条是专门治「按篇幅打分」的。探针（testdata/golden）里量出来过：
// 600 字的空话能拿 6 分、90 字说到点子上的只得 3 分——它是在数长度，不是在读内容。
// 所以这条必须写在协议里，而且要在最前面，不能塞在末尾当补充说明。
//
// 硬规矩第 2 条是治「不敢给满分」的：不让它自己发明扣分理由，
// 它就没有"我觉得还不够完美，扣一点"这个出口，零命中才会真的等于 A 档。
const gradeProtocolTemplate = `下面是你和候选人的一段问答。请当考官判卷，判断这份回答踩了哪些坑。

硬规矩：
1. 篇幅长短完全不参与判断。写得长不等于答得好，写得短不等于答得差。
   通篇空话的长回答该扣照样扣，两句话说到点子上的短回答不该扣。
2. 只判断下面列出的这几条。不要自己发明扣分理由，不要因为「不够完美」就多加。
3. 一条都没踩就是 []。这是正常情况，好回答本来就不该有扣分项。

<<<JUDGE
{"hits": ["name_only", "no_number"], "evidence": {"name_only": "通篇只说了这个名词在做什么"}, "depth": 2, "suspect": ""}
JUDGE>>>

<<<REVIEW
{"comment": "答得偏浅，只讲了怎么做没讲为什么", "weak_points": [{"tag": "表达/只讲做了什么没讲为什么", "detail": "讲了用了什么方案，没讲为什么不用别的"}]}
REVIEW>>>

必须遵守：
1. 两块标记原样写，不许改成代码块，也不许用 HTML 注释。
2. 每块里是一行能直接读的 JSON，结尾不要多逗号，引号用普通双引号。
3. hits 里每个 id 必须从下面这份扣分项清单里原样抄一个：
   一个字都不许改、不许加括号备注、不许自己造近义词。
4. evidence 可以只写一部分，也可以写 {}；写了就要贴原文，一句话。
5. depth 判的是「他把这个问题讲到了第几层」，只能填 0 到 3：
   0 = 基本没答，或者答非所问
   1 = 只说了是什么（报了个名词、定义、结论）
   2 = 讲清了为什么这么做，或者具体怎么做
   3 = 连边界、代价、取舍也讲到了
   depth 不参与打分，只用来量掌握程度，所以你照实填，不用担心扣分。
6. suspect 只在「这句话可疑」时写一句话（像是背的、明显夸大、和前面矛盾），
   没有就写空字符串 ""。不要为了填满而编。
7. weak_points 写 1 到 3 条，确实没有明显毛病就写 []。
   写能力上的毛病，不要写"这题没答好"这种。
8. 每条的 tag 必须从下面这份标签清单里原样挑一个：一个字都不许改、
   不许加括号备注、不许加后缀、不许自己造近义词。
   实在没有一个对得上，tag 就写 "未归类"。
9. 每条的 detail 写这次具体差在哪，一句话，要能让人照着复盘。

可用扣分项：
<可用扣分项>

可用标签：
<可用标签>`

// GradeSystemPrompt 拼出完整的判卷提示词（协议 + 扣分项清单 + 标签清单）。
//
// 两份清单都是运行时填进去的，不写死在模板里——
// 以后加标签只改 tags.txt，调扣分项只改 rubric.go，都不用动这段提示词。
func GradeSystemPrompt(tags *TagBook) string {
	s := strings.Replace(gradeProtocolTemplate, gradeDeductSlot, DeductionPromptList(), 1)
	return strings.Replace(s, gradeTagSlot, tags.PromptList(), 1)
}
