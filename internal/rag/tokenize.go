package rag

import (
	"strings"
	"unicode"
)

// Terms 把一段文本拆成"检索用的词"。
//
// 中文没有空格，不能按空格切。这里用最简单但有效的办法：
// 汉字按相邻两字一组切（bigram），英文和数字按整词切。
//
// 为什么不上分词库：
//  1. 检索不需要"真的会分词"，只需要查询和资料用同一套切法——
//     两边对得上就够，切得对不对根本不重要
//  2. bigram 对技术术语特别友好："goroutine 泄漏" 和 "泄漏 goroutine"
//     会切成同一批片段，反过来也配对得上，不依赖任何词典
//  3. 少一个第三方依赖，少一个"版本升级后分词结果变了"的坑
//
// 为什么不需要停用词表：「的」「是」这类词几乎每个 chunk 里都有，
// 倒排里的 IDF 会把它自动压到接近 0，不用手写黑名单。
func Terms(s string) []string {
	s = strings.ToLower(s)

	var (
		out  []string
		han  []rune // 攒连续的汉字
		word []rune // 攒连续的英文/数字
	)

	flushHan := func() {
		if len(han) >= 2 {
			// 相邻两字一组：神经/网络 → 神经、经网、网络
			for i := 0; i+1 < len(han); i++ {
				out = append(out, string(han[i:i+2]))
			}
		} else if len(han) == 1 {
			out = append(out, string(han))
		}
		han = han[:0]
	}
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '+':
			flushHan()
			word = append(word, r)
		default:
			// 标点、空格、全角符号：两种都收口
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
	return out
}
