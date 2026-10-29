package interview

import (
	"errors"
	"testing"
)

// 覆盖三级兜底的每条路径：定界块 / 裸 JSON / 修复 / 彻底失败。
func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantErr  error
		wantText string
	}{
		{
			name: "标准定界块",
			raw: "你说得不够具体。\n" + EvalOpen + "\n" +
				`{"score": 7, "should_advance": true, "weak_points": ["没量化收益"], "dimension_scores": {"technical_depth": 7, "project_articulation": 5, "communication": 8, "problem_solving": 6}}` +
				"\n" + EvalClose,
			wantText: "你说得不够具体。",
		},
		{
			name:     "无定界符的裸 JSON",
			raw:      "换个角度问：这个项目的 QPS 是多少？\n" + `{"score": 5, "should_advance": false}`,
			wantText: "换个角度问：这个项目的 QPS 是多少？",
		},
		{
			name: "定界块内包了代码围栏",
			raw: "好。\n" + EvalOpen + "\n```json\n" + `{"score": 6}` + "\n```\n" + EvalClose,
			wantText: "好。",
		},
		{
			name:     "JSON 带尾逗号",
			raw:      "嗯。\n" + EvalOpen + `{"score": 7, "should_advance": true,}` + EvalClose,
			wantText: "嗯。",
		},
		{
			name:     "JSON 被截断",
			raw:      "嗯。\n" + EvalOpen + `{"score": 8, "should_advance": true` + EvalClose,
			wantText: "嗯。",
		},
		{
			name:     "人话里含花括号不能干扰配对",
			raw:      "你这个 {数据结构} 讲得还行\n" + `{"score": 5, "should_advance": false}`,
			wantText: "你这个 {数据结构} 讲得还行",
		},
		{
			name:     "完全没有 JSON",
			raw:      "随便聊聊吧。",
			wantErr:  ErrNoEval,
			wantText: "随便聊聊吧。",
		},
		{
			name:     "定界块内为空",
			raw:      "好的\n" + EvalOpen + "\n\n" + EvalClose,
			wantErr:  ErrBadJSON,
			wantText: "好的",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, eval, err := Parse(tt.raw)

			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErr != nil && eval != nil {
				t.Fatalf("eval = %+v, want nil", eval)
			}
			if text != tt.wantText {
				t.Fatalf("text = %q, want %q", text, tt.wantText)
			}
		})
	}
}

// 覆盖字段级兜底：漏填、越界、null 切片。
func TestNormalize(t *testing.T) {
	t.Run("四维漏填用总分兜底", func(t *testing.T) {
		_, e, err := Parse("好。\n" + EvalOpen + `{"score": 6, "should_advance": false}` + EvalClose)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		d := e.DimensionScores
		if d.TechnicalDepth != 6 || d.Communication != 6 {
			t.Fatalf("四维未用总分兜底: %+v", d)
		}
		if !e.Partial {
			t.Fatal("推断出来的数据必须标记 Partial")
		}
	})

	t.Run("总分漏填用四维均值兜底", func(t *testing.T) {
		_, e, err := Parse("好。\n" + EvalOpen +
			`{"dimension_scores": {"technical_depth": 8, "project_articulation": 6, "communication": 7, "problem_solving": 5}}` + EvalClose)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if e.Score != 6 {
			t.Fatalf("score = %d, want 6", e.Score)
		}
		if !e.Partial {
			t.Fatal("推断出来的数据必须标记 Partial")
		}
	})

	t.Run("越界分数被夹到 0-10", func(t *testing.T) {
		_, e, err := Parse("好。\n" + EvalOpen +
			`{"score": 15, "dimension_scores": {"technical_depth": 20, "project_articulation": -3, "communication": 9, "problem_solving": 9}}` + EvalClose)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if e.Score != 10 || e.DimensionScores.TechnicalDepth != 10 || e.DimensionScores.ProjectArticulation != 0 {
			t.Fatalf("clamp 失败: %+v", e)
		}
	})

	t.Run("weak_points 为 null 时给空切片", func(t *testing.T) {
		_, e, err := Parse("好。\n" + EvalOpen +
			`{"score": 5, "weak_points": null, "dimension_scores": {"technical_depth": 5, "project_articulation": 5, "communication": 5, "problem_solving": 5}}` + EvalClose)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if e.WeakPoints == nil {
			t.Fatal("WeakPoints 不能是 nil，否则下游序列化成 null")
		}
	})

	t.Run("正常数据不带 Partial 标记", func(t *testing.T) {
		_, e, err := Parse("好。\n" + EvalOpen +
			`{"score": 7, "should_advance": true, "weak_points": ["没量化"], "dimension_scores": {"technical_depth": 7, "project_articulation": 6, "communication": 8, "problem_solving": 7}}` + EvalClose)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if e.Partial {
			t.Fatal("正常数据不该被标记 Partial")
		}
		if !e.ShouldAdvance || len(e.WeakPoints) != 1 {
			t.Fatalf("字段丢失: %+v", e)
		}
	})
}
