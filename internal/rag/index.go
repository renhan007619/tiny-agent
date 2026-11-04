package rag

import (
	"fmt"
	"math"
	"sort"
)

// Hit 是一条检索结果。
type Hit struct {
	Chunk Chunk
	Score float64
}

// Index 是倒排索引 + 打分要用的统计量。
//
// 先把一件容易被误解的事说清楚：这里的"向量"用的是词的 TF-IDF 权重，
// **不是神经网络 embedding**。所以它和 BM25 是近亲，打分结果会高度相关。
// 这不是写坏了，是诚实——手上没有 embedding 服务时，先把接口和评测立起来，
// 等真 embedding 进来，加一个 Retriever 实现就能对比，别的都不用动。
type Index struct {
	chunks  []Chunk
	tfs     []map[string]int // 每块里每个词出现几次
	lens    []int            // 每块有多少个词
	avgLen  float64          // 平均长度，BM25 算长文档惩罚要用
	posting map[string][]int // 词 -> 出现过它的块下标（去重）
	df      map[string]int   // 词 -> 出现在多少块里（算 IDF 用）
}

// BuildIndex 建索引。
func BuildIndex(chunks []Chunk) *Index {
	ix := &Index{
		chunks:  chunks,
		tfs:     make([]map[string]int, len(chunks)),
		lens:    make([]int, len(chunks)),
		posting: make(map[string][]int),
		df:      make(map[string]int),
	}

	total := 0
	for i, c := range chunks {
		// 把 Title 拼进正文一起索引：这是"免费上下文"。
		// 面经里"自我介绍。"只有五个字，光看正文谁也搜不到；
		// 但它的标题里有"腾讯 T5"，拼上之后"腾讯问过什么"就能命中它。
		terms := Terms(c.Title + " " + c.Text)

		tf := make(map[string]int, len(terms))
		for _, t := range terms {
			tf[t]++
		}
		ix.tfs[i] = tf
		ix.lens[i] = len(terms)
		total += len(terms)

		for t := range tf {
			ix.posting[t] = append(ix.posting[t], i)
			ix.df[t]++
		}
	}
	if len(chunks) > 0 {
		ix.avgLen = float64(total) / float64(len(chunks))
	}
	return ix
}

// Chunks 返回原始数据（只读），评测和报告要用。
func (ix *Index) Chunks() []Chunk { return ix.chunks }

// Stats 打印索引规模。排查"是不是根本没切出东西"第一眼看它。
func (ix *Index) Stats() string {
	return fmt.Sprintf("chunk %d 个 | 词表 %d | 平均长度 %.1f 词",
		len(ix.chunks), len(ix.df), ix.avgLen)
}

// idf 用 BM25 的写法：一个词出现在越少的块里，它越值钱。
//
// 加 1 是为了永不为负——df 等于总数（比如"的"）时，log 里的值仍然 > 0。
func (ix *Index) idf(t string) float64 {
	n := float64(len(ix.chunks))
	df := float64(ix.df[t])
	return math.Log(1 + (n-df+0.5)/(df+0.5))
}

// candidates 用倒排先拿候选，避免每次都扫全部块。
//
// 这是索引存在的意义：资料从几十 KB 长到几 MB 时，只有含查询词的块
// 需要算分，其余连看都不用看。
func (ix *Index) candidates(qs []string) map[int]bool {
	out := make(map[int]bool)
	for _, q := range qs {
		for _, i := range ix.posting[q] {
			out[i] = true
		}
	}
	return out
}

// Lexical 是**词法检索**，用 BM25 打分。
//
// 一句话说清 BM25 在干什么：一个块分高，是因为
//   ① 查询里的词它都出现了（命中数）
//   ② 少见词出现比常见词更值钱（IDF）
//   ③ 同一个词出现多次有帮助，但收益递减（词频饱和）
//   ④ 长块不许靠"字多"白占便宜（长度归一化）
func (ix *Index) Lexical(query string, k int) []Hit {
	qs := Terms(query)
	cand := ix.candidates(qs)
	if len(cand) == 0 {
		return nil
	}

	const k1, b = 1.5, 0.75 // k1 控制词频饱和速度，b 控制长度惩罚力度

	var out []Hit
	for i := range cand {
		var score float64
		for _, q := range qs {
			tf := float64(ix.tfs[i][q])
			if tf == 0 {
				continue
			}
			dl := float64(ix.lens[i])
			score += ix.idf(q) * tf * (k1 + 1) / (tf + k1*(1-b+b*dl/ix.avgLen))
		}
		if score > 0 {
			out = append(out, Hit{Chunk: ix.chunks[i], Score: score})
		}
	}
	return topK(out, k)
}

// Vector 是**向量检索**，用 TF-IDF 权重算余弦相似度。
//
// 和 BM25 的区别在怎么看：这里真的把两块文本都当成向量，
// 查询也归一化，最后量的是两个向量夹角的余弦（0~1，越大越像）。
// 权重的用法也不同：这里用 (1+log tf) 压住长文档，再乘 IDF。
func (ix *Index) Vector(query string, k int) []Hit {
	qs := Terms(query)
	cand := ix.candidates(qs)
	if len(cand) == 0 {
		return nil
	}

	// 查询侧向量
	qTF := make(map[string]int, len(qs))
	for _, q := range qs {
		qTF[q]++
	}
	qVec := make(map[string]float64, len(qTF))
	var qNorm float64
	for t, tf := range qTF {
		w := (1 + math.Log(float64(tf))) * ix.idf(t)
		qVec[t] = w
		qNorm += w * w
	}
	qNorm = math.Sqrt(qNorm)
	if qNorm == 0 {
		return nil
	}

	var out []Hit
	for i := range cand {
		var dot, dNorm float64

		// 先算"查询命中的那几个词"贡献的点积
		for t := range qVec {
			tf := ix.tfs[i][t]
			if tf == 0 {
				continue
			}
			w := (1 + math.Log(float64(tf))) * ix.idf(t)
			dot += qVec[t] * w
			dNorm += w * w
		}
		if dot == 0 {
			continue
		}

		// 再补上"没被查询命中、但向量里本来就有"的那些维度。
		// 少了这一步，长块的向量长度会被算短，余弦值虚高——
		// 这是手写余弦最容易漏的一段。
		for t, tf := range ix.tfs[i] {
			if _, hit := qVec[t]; hit {
				continue
			}
			w := (1 + math.Log(float64(tf))) * ix.idf(t)
			dNorm += w * w
		}
		dNorm = math.Sqrt(dNorm)
		if dNorm == 0 {
			continue
		}

		out = append(out, Hit{Chunk: ix.chunks[i], Score: dot / (qNorm * dNorm)})
	}
	return topK(out, k)
}

// rrfK 是倒数排名融合的平滑常数，业界常用 60。
const rrfK = 60.0

// Hybrid 是**双路召回**：词法一路 + 向量一路，用 RRF 把两路名次合起来。
//
// 为什么不用"分数相加"：BM25 的分是 0~20 量级，余弦是 0~1，
// 直接相加等于让 BM25 一个人说了算。RRF 只看名次不看分数，
// 天生免疫"两边量纲不一样"这个问题。
//
// 合出来的直觉：两路都排在前面的块，分自然叠加得最高——
// 这就是"两个不同角度看都相关"的意思，比单路第一名更可信。
func (ix *Index) Hybrid(query string, k int) []Hit {
	merged := make(map[string]float64)
	byID := make(map[string]Chunk)

	// 名次从 1 开始：第 1 名得 1/61，第 10 名得 1/70。
	// 各取 4k 条候选再合，避免"某一路的第 8 名比另一路的第 1 名还靠前"时被截断掉。
	for rank, h := range ix.Lexical(query, k*4) {
		merged[h.Chunk.ID] += 1 / (rrfK + float64(rank) + 1)
		byID[h.Chunk.ID] = h.Chunk
	}
	for rank, h := range ix.Vector(query, k*4) {
		merged[h.Chunk.ID] += 1 / (rrfK + float64(rank) + 1)
		byID[h.Chunk.ID] = h.Chunk
	}

	out := make([]Hit, 0, len(merged))
	for id, s := range merged {
		out = append(out, Hit{Chunk: byID[id], Score: s})
	}
	return topK(out, k)
}

// topK 按分数降序取前 k 条。同分按 ID 排，保证结果稳定可复现——
// 不然 map 的随机遍历顺序会让同一句话跑两次给出不同结果。
func topK(hits []Hit, k int) []Hit {
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].Chunk.ID < hits[b].Chunk.ID
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
