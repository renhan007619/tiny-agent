// ragcheck 是 RAG 的尺子：建索引、查一句、跑评测。
//
// 名字照着 cmd/evalcheck 取——两个命令合起来构成一个习惯：
// 改任何东西之前，先能把它量出来。没有尺子的改动只能"感觉好像好点了"。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"tiny-agent/internal/rag"
)

// docs 是资料清单，写死在代码里而不自动扫全目录。
//
// 为什么不扫全目录：工作区里还有 .git、数据库文件、testdata、cmd 本身，
// 全扫会把垃圾也建成索引，检索结果里混进一堆不该出现的东西。
// 加资料就改这张表——一次显式的决定，比一个聪明的扫描规则可靠。
var docs = []rag.Doc{
	{Path: "学习指南/agent面经.md", Source: rag.SourceInterview},
	{Dir: "学习指南/项目踩坑", Source: rag.SourcePitfall},
	{Dir: "学习指南/面试官项目", Source: rag.SourceProject},
}

// expect 是评测集里的一条：这句查询，应该命中哪个文件的哪段文字。
//
// 为什么不记行号：行号会随编辑漂移，今天写的 412，明天改两行就变成 414，
// 评测前还得先维护评测集本身。记"正文里必须有的一段字"就不会漂。
type expect struct {
	Query string `json:"query"`
	File  string `json:"file"`
	Must  string `json:"must"`
	Note  string `json:"note"`
}

func main() {
	q := flag.String("q", "", "查一句话，看三路检索各返回什么")
	evalPath := flag.String("eval", "", "评测集 json 路径，跑 Recall@k 对比")
	topN := flag.Int("n", 3, "每次返回几条")
	flag.Parse()

	chunks, err := rag.Load(docs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读资料失败:", err)
		os.Exit(1)
	}
	if len(chunks) == 0 {
		fmt.Fprintln(os.Stderr, "一条 chunk 都没切出来，检查 docs 里的路径（要在项目根目录跑）")
		os.Exit(1)
	}

	ix := rag.BuildIndex(chunks)
	fmt.Println("索引：", ix.Stats())

	switch {
	case *q != "":
		show(ix, *q, *topN)
	case *evalPath != "":
		runEval(ix, *evalPath, *topN)
	default:
		// 不带参数时给一个自检样例：能跑出结果，就说明整条链路是通的。
		show(ix, "分块策略", *topN)
	}
}

// retrievers 是三路检索的统一名字。加新路（比如真 embedding）只往这里加一项。
func retrievers(ix *rag.Index) []struct {
	Name  string
	Fetch func(query string, k int) []rag.Hit
} {
	return []struct {
		Name  string
		Fetch func(query string, k int) []rag.Hit
	}{
		{"BM25", ix.Lexical},
		{"余弦", ix.Vector},
		{"融合", ix.Hybrid},
	}
}

// show 把三路结果并排打出来。
//
// 为什么要并排看：同一句话三路给出不同结果，才看得出"哪一路漏了、
// 哪一路在瞎猜"。只跑一路，你永远不知道它是准还是碰巧。
func show(ix *rag.Index, query string, k int) {
	fmt.Printf("\n=== 查询：%s ===\n", query)

	for _, r := range retrievers(ix) {
		hits := r.Fetch(query, k)
		if len(hits) == 0 {
			fmt.Printf("\n[%s] 无结果\n", r.Name)
			continue
		}
		fmt.Printf("\n[%s]\n", r.Name)
		for i, h := range hits {
			fmt.Printf("  %d. %s:%d  (%.3f)\n     %s\n",
				i+1, h.Chunk.File, h.Chunk.Line, h.Score, clip(h.Chunk.Text, 60))
		}
	}
}

// runEval 跑评测集，输出三路的 Recall@1 / Recall@3 / MRR。
//
// 这三个数各自回答一个问题：
//
//	Recall@1  第一眼看到的就是对的吗（用户最在乎的）
//	Recall@3  前三里能不能找到（够不够用来做"建议材料"）
//	MRR       平均倒数排名（第一名的位置有多靠前，比 Recall 更细）
func runEval(ix *rag.Index, path string, k int) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读评测集失败:", err)
		os.Exit(1)
	}
	var cases []expect
	if err := json.Unmarshal(data, &cases); err != nil {
		fmt.Fprintln(os.Stderr, "解析评测集失败:", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "评测集是空的")
		os.Exit(1)
	}

	type stat struct {
		n1    int // 第一名命中
		n3    int // 前三命中
		found int // 前 k 里有命中
		rank  int // 命中名次之和，算 MRR 用
	}
	stats := make(map[string]*stat, 3)
	for _, r := range retrievers(ix) {
		stats[r.Name] = &stat{}
	}

	for _, c := range cases {
		for _, r := range retrievers(ix) {
			hits := r.Fetch(c.Query, k)
			rank := matchRank(hits, c)
			if rank == 0 {
				continue
			}
			s := stats[r.Name]
			s.found++
			s.rank += rank
			if rank == 1 {
				s.n1++
			}
			if rank <= 3 {
				s.n3++
			}
		}
	}

	n := float64(len(cases))
	fmt.Printf("\n=== 评测集 %d 条，取前 %d ===\n", len(cases), k)
	fmt.Printf("%-6s %9s %9s %7s %9s\n", "检索", "Recall@1", "Recall@3", "MRR", "平均名次")
	for _, r := range retrievers(ix) {
		s := stats[r.Name]
		mrr, avgRank := 0.0, 0.0
		if s.found > 0 {
			avgRank = float64(s.rank) / float64(s.found)
			mrr = 1 / avgRank
		}
		fmt.Printf("%-6s %8.0f%% %8.0f%% %7.2f %9.1f\n",
			r.Name,
			float64(s.n1)/n*100,
			float64(s.n3)/n*100,
			mrr,
			avgRank,
		)
	}
	fmt.Println("\n（没命中的查询不会算进平均名次，所以 MRR 偏高是正常的）")
}

// matchRank 返回正确答案在第几名，0 表示前 k 里都没有。
//
// 判定要同时满足两条：文件对得上、正文里确实有那段字。
// 只看文件不够（一个文件几百块），只看字也不够（同样的字可能到处都有）。
func matchRank(hits []rag.Hit, c expect) int {
	for i, h := range hits {
		if !strings.HasSuffix(h.Chunk.File, c.File) {
			continue
		}
		if c.Must == "" ||
			strings.Contains(h.Chunk.Text, c.Must) ||
			strings.Contains(h.Chunk.Title, c.Must) {
			return i + 1
		}
	}
	return 0
}

// clip 把一段话压成一行，用来打印预览。
func clip(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
