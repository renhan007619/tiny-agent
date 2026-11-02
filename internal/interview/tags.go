package interview

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// 标签库里两个系统保留的名字。
const (
	// TagUnclassified 是最后的兜底：模型给的名字在库里完全找不到。
	// 它占突出的比重，就说明标签库该补了，或者 prompt 里的清单模型没看懂。
	TagUnclassified = "未归类"

	// TagUnspecified 接在父级后面，表示"知道是这一块，但没说清是哪一点"。
	// 例如模型只说"并发"，库里只有"并发/channel 关闭与阻塞"这种细项，
	// 那就记成 "并发/未细分"，既进了账，又不假装知道细节。
	TagUnspecified = "未细分"
)

// TagBook 是标签册子。
//
// 它只干一件事：保证"记进库里的标签"永远是库里存在的那几个之一。
// 它不判断分类对不对——分类是模型的事，这里只负责把名字对齐到库里。
//
// 两个字段的分工：
//
//	byPath  —— 这个名字库里有没有
//	parents —— 哪些名字只是"筐"（下面还挂着子项的，不能直接当计数格子）
type TagBook struct {
	all     []string        // 全部标签，保持文件里的顺序（决定展示顺序）
	byPath  map[string]bool // 名字存不存在
	parents map[string]bool // 哪些是父级（有子标签的那种）
}

// LoadTags 从文件里读出标签库。
//
// 这个文件是人手写的，程序只读不写——所以重复、空行这些都得在"读进来那一刻"
// 自己挡掉，不能指望写的人不出错，也不能回写去"帮它整理"（会毁掉人排的顺序和注释）。
func LoadTags(path string) (*TagBook, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开标签库失败: %w", err)
	}
	defer f.Close()

	book := &TagBook{byPath: map[string]bool{}, parents: map[string]bool{}}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue // 空行和注释跳过
		}
		if book.byPath[line] {
			continue // 重复的不要
		}
		book.byPath[line] = true
		book.all = append(book.all, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(book.all) == 0 {
		return nil, fmt.Errorf("标签库 %s 是空的", path)
	}

	// 凡是"被别的标签用 / 挂在后面"的，都算父级。
	// 例如库里有 "并发/channel 关闭与阻塞"，那 "并发" 就是父级。
	//
	// 按斜杠切成段，把每一段的前缀都记一遍：
	//   "并发/channel 关闭与阻塞" → parents["并发"] = true
	//   "a/b/c"（以后真做了三级） → parents["a"] = true, parents["a/b"] = true
	for _, tag := range book.all {
		parts := strings.Split(tag, "/")
		for i := 1; i < len(parts); i++ {
			book.parents[strings.Join(parts[:i], "/")] = true
		}
	}
	return book, nil
}

// All 返回全部标签。
func (b *TagBook) All() []string { return b.all }

// PromptList 把标签拼成一段给模型看的清单。
//
// 按层级缩进两格，模型一眼能看出谁是谁的子项。
// 顺序就是文件里的顺序——你想让哪一组排前面，就把它挪到文件上面去。
func (b *TagBook) PromptList() string {
	var sb strings.Builder
	for _, tag := range b.all {
		depth := strings.Count(tag, "/")
		sb.WriteString(strings.Repeat("  ", depth))
		sb.WriteString(tag)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Resolve 把模型给的名字，变成一个"库里一定存在的、可以计数的"标签。
//
// 为什么要有它：模型偶尔会把名字写歪（多个空格、加个括号、写个近义词）。
// 这一步不判断分类对不对，只负责把名字对到库里去。
//
// 三条规则，从上到下试：
//  1. 精确命中，而且它自己不是父级 → 就用它（这是最理想的情况）
//  2. 逐级往上砍，砍到库里有的那一级：
//     砍到的是细项 → 用它；砍到的是父级 → 用 "父级/未细分"
//  3. 都不行 → 「未归类」
//
// 故意做"笨"：它不会剥括号、不会模糊匹配、不会脑补对齐。
// 因为一旦它聪明到能圆上模型所有的乱写，你就永远发现不了 prompt 有问题。
// 「未细分」出现得多，本身就是"该改 prompt 或补标签"的信号。
func (b *TagBook) Resolve(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return TagUnclassified
	}

	// ① 精确命中，而且它自己不是父级（不是父级 = 最细的一级，能计数）
	if b.byPath[s] && !b.parents[s] {
		return s
	}

	// ② 逐级往上砍：先砍到 "并发/xxx"，再砍到 "并发"
	t := s
	for {
		i := strings.LastIndex(t, "/")
		if i <= 0 {
			break // 没有斜杠了，或者斜杠在开头（病态写法），没得砍
		}
		t = t[:i] // 砍掉最后一段

		if !b.byPath[t] {
			continue // 这一级库里没有，继续往上砍
		}
		if !b.parents[t] {
			return t // 是能计数的细项，就用它
		}
		// 是父级：记到它下面那个"说不清是哪一点"的格子。
		// 这里直接返回、不再往上砍——再往上就太粗了，会丢掉区分度。
		return t + "/" + TagUnspecified
	}

	// ③ 精确命中，但它本身就是个父级（模型只说了"并发"）。
	// 这一步必须单独写，否则会漏掉：第①步因为它 parent 被排除，
	// 第②步因为它没有斜杠一步都退不了，最后会掉进「未归类」——
	// 明明是个对的标签，却被扔进垃圾堆。
	if b.byPath[s] && b.parents[s] {
		return s + "/" + TagUnspecified
	}

	return TagUnclassified
}
