// Package rag 是资料检索：把学习资料切成小块，按一句话找最相关的几块。
//
// 它只做一件事——**指路**：告诉你"你问的这个东西，在哪份文件的第几行"。
// 故意不做"AI 总结代答"：检索错了你能点回原文核对，总结错了你分不清
// 是资料不对还是模型胡说，那会污染你自己的复习材料。
package rag

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Source 是资料的类型。
//
// 为什么要分：同样是"context 超时"，面经讲的是"别人问过什么"，踩坑讲的是
// "我怎么修的"，项目自述讲的是"我该怎么讲"。三类混在一个索引里检索，
// 结果是三碗混在一起的汤——都相关，但都不是你要问的那个。
type Source string

const (
	SourceInterview Source = "面经" // 别人被问过的题
	SourcePitfall   Source = "踩坑" // 我自己踩的坑和修法
	SourceProject   Source = "项目" // 我自己的项目叙述
)

// Doc 描述"哪份资料、算什么类型"。Path 和 Dir 二选一：
//   - Path 指单个文件
//   - Dir 指一个目录，里面所有 .md 都算
type Doc struct {
	Path   string
	Dir    string
	Source Source
}

// Chunk 是检索的最小单位：一小段能独立看懂的原文 + 它是从哪来的。
//
// File 和 Line 是给人看的，不是给程序看的：检索错了，你拿"文件:行号"
// 直接点回原文核对。这一列是"不许瞎编"的物理保障。
type Chunk struct {
	ID     string // 文件:行号，全局唯一，也是去重和融合时的身份
	Source Source
	File   string // 统一用 / 分隔，Windows 上也一样
	Line   int    // 起始行号（从 1 开始）
	Title  string // 所属标题链，例如 "腾讯专题 > T5. 腾讯暑期实习 agent 开发一面"
	Text   string
}

var (
	// headerRe 认 markdown 标题行：# 到 ######。
	headerRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

	// itemRe 认列表项："1. xxx" 或 "- xxx"。
	// 面经里的每道题都是这种形状，一条就是一个独立的知识点。
	itemRe = regexp.MustCompile(`^\s*(?:\d+\.|[-*])\s+(.*)$`)
)

// Load 把一批资料切成 chunk。
func Load(docs []Doc) ([]Chunk, error) {
	var out []Chunk

	for _, d := range docs {
		if d.Path != "" {
			cs, err := SplitFile(d.Path, d.Source)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.Path, err)
			}
			out = append(out, cs...)
			continue
		}

		entries, err := os.ReadDir(d.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			p := filepath.Join(d.Dir, e.Name())
			cs, err := SplitFile(p, d.Source)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			out = append(out, cs...)
		}
	}

	// 太短的丢掉：一个字的标题、空的列表项，进索引只会制造噪音。
	keep := out[:0]
	for _, c := range out {
		if len([]rune(c.Text)) >= 4 {
			keep = append(keep, c)
		}
	}
	return keep, nil
}

// SplitFile 把一个 md 文件切成 chunk。
func SplitFile(path string, src Source) ([]Chunk, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 统一成 / 分隔：Windows 下 ReadDir 给的是反斜杠，
	// 不统一的话同一个文件会出现两种写法，评测集永远匹配不上。
	return splitText(string(data), filepath.ToSlash(path), src), nil
}

// splitText 是切分的全部逻辑，三条规则按优先级：
//
//  1. 遇到标题 → 记住"现在属于哪个标题"（标题栈）
//  2. 遇到列表项（1. 或 -）→ 这一行单独成一块（面经的一道题 = 一块）
//  3. 其余非空行攒成一段，遇空行/标题/列表项就收口
//
// 为什么按结构切而不按字数切：markdown 的标题和列表是作者亲手划的语义边界。
// 按字数硬切会把一道题劈成两半，检索出来一半有上句没下句，
// 模型看到半个问题就只能瞎猜。
func splitText(text, path string, src Source) []Chunk {
	var (
		out    []Chunk
		titles []string // 标题栈：titles[0] 是一级，最后一个是最近的那个
		buf    []string // 攒着的正文
		bufBeg int      // buf 第一行是第几行
	)

	flush := func() {
		if len(buf) == 0 {
			return
		}
		body := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = nil
		if body == "" {
			return
		}
		out = append(out, Chunk{
			ID:     fmt.Sprintf("%s:%d", path, bufBeg),
			Source: src,
			File:   path,
			Line:   bufBeg,
			Title:  strings.Join(titles, " > "),
			Text:   body,
		})
	}

	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r") // 兼容 CRLF 文件

		// 规则 1：标题。先把攒的正文收口，再更新标题栈。
		if m := headerRe.FindStringSubmatch(line); m != nil {
			flush()
			level := len(m[1])
			if level <= len(titles) {
				titles = titles[:level-1] // 同级或更浅：先砍掉更深的
			}
			titles = append(titles, strings.TrimSpace(m[2]))
			continue
		}

		// 空行：一段的结束。
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}

		// 规则 2：列表项单独成块。
		if m := itemRe.FindStringSubmatch(line); m != nil {
			flush()
			out = append(out, Chunk{
				ID:     fmt.Sprintf("%s:%d", path, lineNo),
				Source: src,
				File:   path,
				Line:   lineNo,
				Title:  strings.Join(titles, " > "),
				Text:   strings.TrimSpace(m[1]),
			})
			continue
		}

		// 规则 3：普通正文攒起来。
		if len(buf) == 0 {
			bufBeg = lineNo
		}
		buf = append(buf, strings.TrimSpace(line))
	}

	flush()
	return out
}
