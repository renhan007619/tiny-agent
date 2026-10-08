package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// resultDirDefault 外置结果的根目录。运行期产物，必须进 .gitignore。
	resultDirDefault = ".tiny-agent/results"

	// resultRetention 保留期。超期的"会话目录"整体删除——不做引用计数，
	// 理由见 Cleanup。
	resultRetention = 7 * 24 * time.Hour

	// maxRecallLines 单次召回的"行数"闸门。它只是防模型一次要太狠的粗闸门，
	// 真正生效的是 token 闸门（pageTokens）。两个都要，原因：
	//   只靠行数 -> 一行 8MB 的 JSON 就能撑爆预留；
	//   只靠 token -> 模型没法预估"该读几行"，容易一次读回半页或反复试探。
	maxRecallLines = 400

	// 头尾配比：头部 2 份、尾部 1 份。这是经验值，不是推导值。
	// 为什么尾部一定要留：错误信息、退出码、栈顶、日志最后一行、结论，
	// 天然压在末尾；"只留头"是最容易犯也最贵的错——看着够用，实际把最该
	// 看的那一行砍了，还逼模型多召回一次。
	externalHeadRatio = 2
	externalTailRatio = 1
)
//这是一分工具结果的小票，谁做的？文件在哪
type resultMeta struct {
	Handle    string    `json:"handle"`     // 句柄，形如 res-3
	Tool      string    `json:"tool"`       // 来源工具名（read_file / bash ...）
	File      string    `json:"file"`       // 同目录下的正文文件名
	Bytes     int       `json:"bytes"`      // 字节数：排查"到底存了多大"
	Lines     int       `json:"lines"`      // 行数：翻页提示与"超出范围"判断要用
	Tokens    int       `json:"tokens"`     // 估算 token：账本口径，注意是估算
	CreatedAt time.Time `json:"created_at"` // 落盘时间：人工排查时最有用的一列
}

type ResultStore struct {
	dir        string // 根目录（默认 .tiny-agent/results）
	session    string // 会话子目录名（同时是 GC 的"当前会话"标记）
	pageTokens int    // 单次召回返回的 token 上限，= Budget.ToolResultReserve

	mu       sync.Mutex             // 保护 seq / byHandle（Save 与 Read 并发时才需要）
	seq      int                    // 单调递增序号：句柄与文件名的来源
	byHandle map[string]resultMeta  // 句柄 -> 元数据：唯一索引
}
func NewResultStore(dir, session string, pageTokens int) (*ResultStore, error) {
	if dir == "" {
		dir = resultDirDefault
	}
	if session == "" {
		session = "default" // 兜底：宁可所有会话共用"default"，也不要写到根目录
	}
	if pageTokens <= 0 {
		// 兜底而不是报错：仓库是"可选能力"，构造失败会连累整个 agent 启不来。
		// 退化成默认配额，账本上仍能看出来（召回页大小会与预算不一致）。
		pageTokens = defaultToolResultReserve
	}
	s := &ResultStore{
		dir:        dir,
		session:    session,
		pageTokens: pageTokens,
		byHandle:   map[string]resultMeta{},
	}
	// 先建目录再 reindex：reindex 读的就是这个目录，顺序反了会拿到 IsNotExist。
	if err := os.MkdirAll(s.sessionDir(), 0o755); err != nil {
		return nil, fmt.Errorf("创建外置结果目录失败: %w", err)
	}
	if err := s.reindex(); err != nil {
		return nil, err
	}
	return s, nil


func (s *ResultStore) sessionDir() string { return filepath.Join(s.dir, s.session) }
// reindex 从 *.meta.json 重建句柄索引，并让 seq 接着上次往下走。
//
// 为什么必须抬 seq：句柄是 res-<n>，文件名也带 n。如果重启后从 1 重新编号，
// 就会覆盖上一轮会话里同名文件，而那个句柄可能还活在模型上下文里——模型
// 拿着 res-1 读回来的居然是别的内容。这种"静默读到错内容"比报错危险得多，
// 所以这里宁可有空洞号也不能重号
func (s *ResultStore) reindex() error {
	entries, err := os.ReadDir(s.sessionDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 目录不在 = 这个会话还没有外置记录，不是错误
		}
		return fmt.Errorf("扫描外置结果目录失败: %w", err)
	}
	for _, e := range entries {
		// 只认 meta：正文文件 .txt 与 meta 一一对应，扫两遍没意义
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(s.sessionDir(), e.Name()))
		if rerr != nil {
			continue // 单个 meta 读不动（权限/被删）不该让整个会话起不来
		}
		var m resultMeta
		if json.Unmarshal(b, &m) != nil || m.Handle == "" {
			continue // 损坏的 meta 直接跳过：比崩掉强，正文仍在磁盘上
		}
		s.byHandle[m.Handle] = m
		if n := parseSeq(m.Handle); n > s.seq {
			s.seq = n
		}
	}
	return nil
}

func (s *ResultStore) Save(tool, full string) (string, error) {
	// 锁放在函数级：seq 自增 + 写文件 + 更新索引必须是一个原子动作，
	// 否则两个并发 Save 会拿到同一个 seq，后写的把先写的覆盖掉。
	// 注意这里不会成为性能瓶颈：agent.go 的工具循环是顺序执行的，
	// 真正需要小心的锁在 Read 里（那里读文件在锁外，见下文）。
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	handle := fmt.Sprintf("res-%d", s.seq)
	// 文件名带工具名不是为了程序，是为了人：ls 一下目录就能看出
	// "这次会话读过什么"，排查"为什么模型一直读不到 X"时最省时间。
	file := fmt.Sprintf("%03d-%s.txt", s.seq, safeName(tool))
	if err := os.WriteFile(filepath.Join(s.sessionDir(), file), []byte(full), 0o644); err != nil {
		return "", fmt.Errorf("外置结果落盘失败: %w", err)
	}

	meta := resultMeta{
		Handle: handle, Tool: tool, File: file,
		Bytes: len(full),
		// Lines/Tokens 在写入时就固化：它们是"这条记录的属性"，
		// 每次读取时重算既浪费又可能因口径变化而不一致。
		Lines:     countLines(full),
		Tokens:    estimateTextTokens(full),
		CreatedAt: time.Now(),
	}
	if err := s.writeMeta(meta); err != nil {
		return "", err
	}
	s.byHandle[handle] = meta
	return handle, nil
}

// writeMeta 旁存元数据。
// 先写正文再写 meta：万一中途挂掉，留下的是"有正文没索引"（内容还在，
// 只是暂时不可寻址），而不是反过来"有索引没正文"（句柄指向空气）。
func (s *ResultStore) writeMeta(m resultMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("外置元数据编码失败: %w", err)
	}
	name := filepath.Join(s.sessionDir(), strings.TrimSuffix(m.File, ".txt")+".meta.json")
	if err := os.WriteFile(name, b, 0o644); err != nil {
		return fmt.Errorf("外置元数据落盘失败: %w", err)
	}
	return nil
}