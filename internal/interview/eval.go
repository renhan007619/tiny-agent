// Package interview 是校招模拟面试官的业务包。
//
// D1 只落地 EVAL 协议：模型每轮回话时除了给出人话，
// 还要吐一段结构化判断（分数 / 是否推进 / 盲点 / 四维分）。
// 这份协议是后续状态机（D2）、诊断报告（D3）、SM-2 排期（D7）、
// 失败归因（D10）的共同上游，所以它是全项目第一个被定下来的契约。
package interview

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---------- 协议定界符 ----------

const (
	// EvalOpen / EvalClose 是模型输出里"表格块"的定界符。
	// [面试-细节] 不用 HTML 注释 <!-- -->：模型回答里若出现 "-->",
	// 注释会被提前截断，JSON 直接腰斩。
	EvalOpen  = "<<<EVAL"
	EvalClose = "EVAL>>>"
)

// ---------- 协议数据结构 ----------

// DimensionScores 四个维度，用来回答"差在哪"而不只是"差多少"。
type DimensionScores struct {
	TechnicalDepth      int `json:"technical_depth"`      // 技术深度
	ProjectArticulation int `json:"project_articulation"` // 项目表达
	Communication       int `json:"communication"`        // 沟通表达
	ProblemSolving      int `json:"problem_solving"`      // 问题求解
}

// Evaluation 是模型每轮必须填的那张表。
type Evaluation struct {
	Score           int             `json:"score"`            // 0-10
	ShouldAdvance   bool            `json:"should_advance"`   // 是否进入下一阶段
	WeakPoints      []string        `json:"weak_points"`      // 暴露的盲点
	DimensionScores DimensionScores `json:"dimension_scores"` // 四维分

	// Partial 不是模型给的字段，是解析器兜底时自己打的标记。
	// true 表示这份数据含推断成分：D3 报告要标注，D7 排期要降权。
	// [面试-架构] 兜底不能假装成功，必须留下"我猜过"的痕迹。
	Partial bool `json:"-"`
}

// ---------- 错误 ----------

var (
	// ErrNoEval 模型压根没输出表格，人话可以直接用，但这一轮没有判断。
	ErrNoEval = errors.New("模型未输出 EVAL 块")
	// ErrBadJSON 找到了表格但 JSON 修不好，必须降级（事后补打分）。
	ErrBadJSON = errors.New("EVAL 块 JSON 无法解析")
)

// ---------- 主解析入口 ----------

// Parse 把模型的一次输出拆成两份东西：
//
//	text —— 给用户看的人话
//	eval —— 给程序用的表格，nil 表示彻底失败、调用方必须降级
//
// 三级兜底：定界块 → 全文最后一个括号平衡对象 → 修 JSON。
func Parse(raw string) (string, *Evaluation, error) {
	text := raw
	var payload string

	if p, start, end, ok := blockSpan(raw); ok {
		// 一级：标准定界块
		text = raw[:start] + raw[end:]
		payload = p
	} else if p, start, end, ok := lastObjectSpan(raw); ok {
		// 兜底 A：模型忘了写定界符，退而求其次找裸 JSON
		text = raw[:start] + raw[end:]
		payload = p
	} else {
		// 彻底失败：人话照常返回，表格交给上层事后补
		return strings.TrimSpace(raw), nil, ErrNoEval
	}

	text = strings.TrimSpace(text)

	var e Evaluation
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		// 兜底 B：JSON 本身坏了（代码围栏 / 尾逗号 / 截断）
		if err2 := json.Unmarshal([]byte(repairJSON(payload)), &e); err2 != nil {
			return text, nil, fmt.Errorf("%w: %v", ErrBadJSON, err)
		}
		e.Partial = true
	}

	// 兜底 C：字段级缺失 / 越界
	e.normalize()
	return text, &e, nil
}

// ---------- 定界块提取 ----------

// blockSpan 取最后一个定界块并返回它在原文里的区间。
//
// [面试-细节] 取最后一个而不是第一个：prompt 里的 few-shot 示例
// 也可能含同样的定界符，模型的复述会污染第一次匹配。
func blockSpan(raw string) (payload string, start, end int, ok bool) {
	closeIdx := strings.LastIndex(raw, EvalClose)
	if closeIdx < 0 {
		return "", 0, 0, false
	}
	openIdx := strings.LastIndex(raw[:closeIdx], EvalOpen)
	if openIdx < 0 {
		return "", 0, 0, false
	}
	return strings.TrimSpace(raw[openIdx+len(EvalOpen) : closeIdx]),
		openIdx, closeIdx + len(EvalClose), true
}

// lastObjectSpan 正向扫描，记住最后一个顶层 {...} 的区间。
//
// [面试-细节] 必须跟踪字符串与转义状态：回答正文里的 "{" 或 "\""
// 会让纯数括号的配对全错。
func lastObjectSpan(s string) (payload string, start, end int, ok bool) {
	var stack []int
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, i)
		case '}':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				payload, start, end, ok = s[open:i+1], open, i+1, true
			}
		}
	}
	return payload, start, end, ok
}

// ---------- JSON 修复 ----------

var trailingCommaRe = regexp.MustCompile(`,(\s*[}\]])`)

// repairJSON 只修三种最常见坏法，修不了就原样返回、让上层报错。
//
// [面试-架构] 不做全量 JSON 修复器：逞强的修复器会静默产出错数据，
// 比直接报错危险十倍。
func repairJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```JSON")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// 尾逗号：{"a": 1,}
	s = trailingCommaRe.ReplaceAllString(s, "$1")
	// 被截断：补右括号。这里按字符数粗补，够用即可。
	for strings.Count(s, "{") > strings.Count(s, "}") {
		s += "}"
	}
	return s
}

// ---------- 字段级归一 ----------

// normalize 保证下游拿到的是"能直接用"的数据，而不只是"解析成功"。
//
// [面试-架构] 兜底要兜到下游能安全消费为止：nil 切片序列化成 null，
// 前端 .map() 直接崩。
func (e *Evaluation) normalize() {
	e.Score = clamp(e.Score, 0, 10)

	d := &e.DimensionScores
	d.TechnicalDepth = clamp(d.TechnicalDepth, 0, 10)
	d.ProjectArticulation = clamp(d.ProjectArticulation, 0, 10)
	d.Communication = clamp(d.Communication, 0, 10)
	d.ProblemSolving = clamp(d.ProblemSolving, 0, 10)

	sum := d.TechnicalDepth + d.ProjectArticulation + d.Communication + d.ProblemSolving

	// 四个维度全 0 但总分 > 0：判定模型漏填，用总分兜底。
	//
	// [面试-细节] 已知取舍：int 无法区分"真 0 分"和"字段缺失"。
	// 要严谨得用 *int，代价是每处取值都要解引用；第一版选简单。
	if sum == 0 && e.Score > 0 {
		d.TechnicalDepth, d.ProjectArticulation = e.Score, e.Score
		d.Communication, d.ProblemSolving = e.Score, e.Score
		e.Partial = true
	}
	// 反过来：总分缺失但四维有值，取均值。
	if e.Score == 0 && sum > 0 {
		e.Score = sum / 4
		e.Partial = true
	}

	if e.WeakPoints == nil {
		e.WeakPoints = []string{}
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------- prompt 那半边 ----------

// EvalProtocol 拼进 system prompt，规定"模型怎么吐表格"。
//
// [面试-架构] schema 有两端：Go struct 与这段指令必须字字对齐。
// 只写 struct 不写指令 = tiny-agent 踩过的老坑（模型不知道有这个格式，
// 永远不吐）。五条硬约束每一条都对应 struct 的一个字段。
const EvalProtocol = `你每轮回复必须严格按以下结构输出，不得改动定界符：

<你的追问或反馈，用中文；先说内容，不要在这里提分数>

<<<EVAL
{"score": 7, "should_advance": true, "weak_points": ["没量化项目收益"], "dimension_scores": {"technical_depth": 7, "project_articulation": 5, "communication": 8, "problem_solving": 6}}
EVAL>>>

硬约束：
1. 定界符原样写成 <<<EVAL 和 EVAL>>>，不许改成代码块，也不许用 HTML 注释。
2. JSON 是单行合法 JSON，不带尾逗号，引号一律用普通双引号。
3. score 为 0-10 的整数；四个维度必须全部填写，不许省略。
4. should_advance 只表示"是否进入下一阶段"，不要输出阶段名。
5. weak_points 只写"能力缺陷"（如"没量化项目收益"），不写"这题没答好"。`
