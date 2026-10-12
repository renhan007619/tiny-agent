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

	mu       sync.Mutex            // 保护 seq / byHandle（Save 与 Read 并发时才需要）
	seq      int                   // 单调递增序号：句柄与文件名的来源
	byHandle map[string]resultMeta // 句柄 -> 元数据：唯一索引
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
}

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

// Read 按行窗口读回全文。offset 从 1 开始。
//
// 两个闸门叠在一起：行数（limit / maxRecallLines）+ token（pageTokens）。
// 少了后者，召回结果自己又会被外层闸门再外置一次（套娃：为了读一个外置
// 结果，又产生一个新的外置结果）。
func (s *ResultStore) Read(handle string, offset, limit int) (string, error) {
	// 只在取 meta 时持锁，读文件在锁外：否则一次大文件读取会卡住所有 Save。
	// 这里的 meta 是值拷贝，出了临界区就与索引脱钩，不会被并发修改影响。
	s.mu.Lock()
	meta, ok := s.byHandle[handle]
	s.mu.Unlock()

	if !ok {
		// 关键：失败必须"吵"，绝不能返回空字符串。
		// 返回空的后果是模型以为"这个文件是空的"，然后基于"空"继续推理——
		// 静默的空内容是外置类功能最危险的失败形态。
		// 顺带把可用句柄列出来，给模型一个自我纠正的抓手。
		return "", fmt.Errorf("句柄 %q 无效或已过期；本会话可用句柄：%s", handle, s.knownHandles())
	}

	f, err := os.Open(filepath.Join(s.sessionDir(), meta.File))
	if err != nil {
		// 索引在但文件没了 = 被 GC 或被人工删了：同样要明确报，不能静默
		return "", fmt.Errorf("打开外置结果失败（文件可能已被清理）: %w", err)
	}
	defer f.Close()

	// ---- 参数 clamp：模型给的参数不可信，一律夹到合法区间 ----
	// offset 从 1 开始是刻意的：页眉要给模型看"第 1-100 行 / 共 812 行"，
	// 人类和模型的直觉都是 1-based，内部换算只保留这一处。
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = maxRecallLines
	}
	if limit > maxRecallLines {
		limit = maxRecallLines
	}

	// 页眉页脚也要占额度，所以正文只给 3/4。
	// 这是经验值，但方向是确定的：凡是"返回结果自带包装"的地方，
	// 都必须先给包装留出额度，否则又会造出"召回一页反而超配额"的新洞。
	bodyBudget := s.pageTokens * 3 / 4

	var sb strings.Builder
	used, lineNo, kept := 0, 0, 0

	// 按行读：日志/JSON/CSV 都是行结构化的，按行切每页自成语义单元；
	// 按字节切会把一行 JSON 拦腰砍断，模型拿到半行反而更容易幻觉。
	sc := bufio.NewScanner(f)
	// 初始 64KB、上限 8MB：单行可能是超大 JSON。超过 8MB 会 Scan 报错，
	// 那是这条记录形态超出工具能力边界，宁可报错也不静默丢内容。
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for sc.Scan() {
		lineNo++
		if lineNo < offset {
			continue // 跳过的行不占额度，只花 CPU
		}
		if kept >= limit {
			break // 行数闸门先到
		}
		line := sc.Text()
		cost := estimateTextTokens(line) + 1 // +1 = 换行

		// 单行本身就超预算：截断这一行也要给。绝不能返回空——
		// 模型至少要看到"这里有内容、但一行太长"。
		if kept == 0 && cost > bodyBudget {
			sb.WriteString(truncateByTokens(line, bodyBudget))
			sb.WriteByte('\n')
			kept = 1
			break
		}
		// token 闸门：宁可少给几行，也不能超过 pageTokens——
		// 超了就会触发套娃外置（见函数头注释）。
		if kept > 0 && used+cost > bodyBudget {
			break
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
		used += cost
		kept++
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("读取外置结果失败: %w", err)
	}

	if kept == 0 {
		// offset 超出总行数：这不是错误，是"你翻过头了"，如实告知即可。
		// 报 error 会让模型以为工具坏了，可能放弃继续读。
		return fmt.Sprintf("[read_result] 句柄 %s 共 %d 行，offset=%d 超出范围", handle, meta.Lines, offset), nil
	}

	// 页眉要回答模型接下来一定会问的三个问题：这是谁、一共多少、我现在看到哪。
	head := fmt.Sprintf("[read_result] 句柄 %s（源 %s，共 %d 行 / 约 %d token，已外置未计入历史）\n第 %d-%d 行：\n",
		handle, meta.Tool, meta.Lines, meta.Tokens, offset, offset+kept-1)

	// 页脚把"下一页怎么读"写成**可直接抄的完整调用**。
	// 模型不会自己算 offset，也不会记得句柄格式；写"可以继续读"几乎无效，
	// 写 read_result(handle="res-3", offset=101, limit=100) 才有效。
	if next := offset + kept; next <= meta.Lines {
		return head + sb.String() + fmt.Sprintf("\n[还有更多：read_result(handle=%q, offset=%d, limit=%d) 继续读]",
			handle, next, limit), nil
	}
	return head + sb.String() + "\n[已到文件末尾]", nil
}

// readResultArgs read_result 的入参。
// 字段名即模型要填的参数名（schema.go 的 functionSchema 直接读 json tag 和
// description tag 生成 input_schema），所以 tag 是"给模型看的文档"。
type readResultArgs struct {
	Handle string `json:"handle" description:"外置结果句柄，形如 res-3，来自截断标记里的“句柄 res-3”"`
	// 为什么描述里非写"读开头就填 1"：schema.go 的 functionSchema 有个硬约束——
	// Go 参数没有默认值，所有字段都进 required。模型**不可能**省略 offset 让我们
	// 兜底，所以只能把默认语义写进描述，代码里再 clamp。这是"schema 表达力不足
	// 时，把默认值下沉到实现 + 文档"的标准处理。
	Offset int `json:"offset" description:"起始行号，从 1 开始；读开头就填 1"`
	Limit  int `json:"limit" description:"本次读取的行数上限（1-400），实际还会受 token 闸门收窄"`
}

// ReadResult 工具入口：方法值直接交给 newTool 注册。
// 注意 newTool 从函数名推出来的是 "ReadResult"（方法值拿不到小写名），
// 所以 main.go 里必须像 read_file 一样显式覆盖 Name = "read_result"。
func (s *ResultStore) ReadResult(args readResultArgs) (string, error) {
	return s.Read(args.Handle, args.Offset, args.Limit)
}

// knownHandles 列出最近的可用句柄，只服务于"句柄写错"时的错误提示。
// 截断到 8 个：这是给人/模型看的线索，不是全量清单，太长反而淹没有用信息。
func (s *ResultStore) knownHandles() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byHandle) == 0 {
		return "（无）"
	}
	hs := make([]string, 0, len(s.byHandle))
	for h := range s.byHandle {
		hs = append(hs, h)
	}
	sort.Strings(hs) // map 遍历顺序随机，排序后提示稳定，模型更容易比对
	if len(hs) > 8 {
		hs = hs[len(hs)-8:]
	}
	return strings.Join(hs, ", ")
}

// Cleanup 删除超过保留期的"会话目录"，返回删除的目录数。
//
// 为什么不做引用计数：引用方是**模型上下文**，进程根本观测不到；而且计数
// 在重启后无法恢复。做出来只会是一套自欺的账。"保留期 + 删整目录"是能
// 自洽的最简策略。
//
// ★ 隐性耦合（将来改代码前必须先看这段）：判活用的是**目录 ModTime**，
// 它成立的前提是 Save 每次都**新建文件**——新文件会刷新父目录 mtime。
// 如果哪天为了少几个文件改成"追加写同一个文件"，目录 mtime 不再刷新，
// 活跃会话就会被误删。这是外置方案最容易在半年后被自己坑到的地方。
func (s *ResultStore) Cleanup(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 还没有任何会话目录，无事可做
		}
		return 0, err
	}
	deadline := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		// 只处理目录，且永不删当前会话——这一条是"不能错"的底线
		if !e.IsDir() || e.Name() == s.session {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(deadline) {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(s.dir, e.Name())); rerr == nil {
			removed++
		}
	}
	return removed, nil
}

// externalizeToolResult 一条工具结果的"无损化"闸门：
//   - 没超配额：原样返回（绝大多数调用走这条，零 IO）
//   - 超配额：全文落盘，上下文只留 头部 + 中间省略提示 + 尾部 + 句柄标记
//   - 落盘失败：退回 L0 纯截断，并在标记里写明失败（绝不假装无损）
//
// 返回值三元组：(进上下文的内容, 句柄, 是否发生过截断/外置)
// 为什么返回句柄而不是让调用方从标记里正则抠：文案一改，抠法就废。
func externalizeToolResult(store *ResultStore, tool, raw string, quota int) (string, string, bool) {
	used := estimateTextTokens(raw)

	// ① 先比配额再落盘：省 IO。工具结果 99% 是小的，
	//    不能因为"可能超"就给每个结果都写一次盘。
	if used <= quota {
		return raw, "", false
	}

	// ② 没有仓库 = 外置能力不可用（比如某个测试没注入）。
	//    这条分支让整个功能可以分步落地：先把闸门接上（行为与 L0 完全一致），
	//    再建仓库；任何一步出问题都不会让 agent 崩。也让 agent.go 不用写
	//    if m.results != nil。
	if store == nil {
		text, _ := truncateToolResult(raw, quota)
		return text, "", true
	}

	// ③ 落盘失败（磁盘满、权限不足）不假装成功：退回 L0 纯截断，
	//    并且**在标记里写明失败**。理由和"截断必须留痕"一样——
	//    静默的信息损失就是数据损坏。这也是一处诚实边界：
	//    外置不是"一定无损"，它依赖磁盘可写。
	handle, err := store.Save(tool, raw)
	if err != nil {
		text, _ := truncateToolResult(raw, quota)
		return text + "\n…[外置失败，完整内容已丢失: " + err.Error() + "]", "", true
	}

	// 标记要做三件事：说清"这是什么"、给出"怎么读回来"、给出"读回来的确切调用"。
	// 少了最后一项，模型经常回一句"内容被截断了"就停了。
	marker := fmt.Sprintf("\n…[已外置：原始约 %d token / %d 行，句柄 %s；用 read_result(handle=%q, offset=1, limit=200) 按行读回全文]",
		used, countLines(raw), handle, handle)
	ellipsis := "\n…（中间省略）…\n"

	// 正文额度 = 配额 − 标记 − 省略提示 − 8。
	// 那 8 是给三段省略号（truncateByTokens / tailByTokens 各自追加的 "…"）
	// 和"字节/3"在边界上的估算回弹留的余量——与 truncateToolResult 里 −2
	// 同一个理由，这里追加物更多，所以余量更大。
	// 这是"闸门与账本必须同源"的代价：宁可少留 8 token，
	// 也不能出现"外置之后仍然超配额"这种自相矛盾。
	body := quota - estimateTextTokens(marker) - estimateTextTokens(ellipsis) - 8
	if body < 0 {
		body = 0 // 配额小到连包装都放不下：正文归零，但标记必须留
	}
	headTokens := body * externalHeadRatio / (externalHeadRatio + externalTailRatio)
	tailTokens := body - headTokens // 用减法而不是再乘一次：保证头尾之和正好等于 body

	// 砍中间、留两头：头是格式/表头，尾是结论/错误码。
	return truncateByTokens(raw, headTokens) + ellipsis + tailByTokens(raw, tailTokens) + marker, handle, true
}

// tailByTokens 取末尾 n token——truncateByTokens 的反向操作。
//
// 为什么必须有它：现有 truncateByTokens 只会**取头**，而本方案的保留策略
// 是"头 + 尾"。
// 为什么必须做 UTF-8 回退：估算口径是 字节数/3，切点几乎必然落在多字节
// 汉字的中间；切出半个字符会让 JSON 编码和模型输入都出问题（项目在
// truncateByTokens 里已经踩过这个坑）。RuneStart 就是"这个字节是不是一个
// 字符的开头"，不是就往后挪一个字节。
func tailByTokens(s string, tokens int) string {
	if tokens <= 0 {
		return ""
	}
	maxBytes := tokens * 3
	if len(s) <= maxBytes {
		return s
	}
	cut := len(s) - maxBytes
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// countLines 数行数：以 \n 为准，最后一行没有换行符也算一行。
// 不用 strings.Split（会为几十万行分配一个巨大切片），Count 只扫一遍。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// parseSeq 从 "res-12" 抠出 12（reindex 抬 seq 用）。
// 解析失败返回 0：坏的 meta 不该把整个索引搞崩，跳过它更安全。
func parseSeq(handle string) int {
	i := strings.LastIndex(handle, "-")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(handle[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// safeName 把工具名净化成文件名安全片段。
// 工具名来自我们的代码，理论上是安全的；这里只是防御——文件名是要拼进
// 路径的，一旦将来有工具名来自配置/插件，未净化的名字就是路径穿越。
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
	if s == "" {
		return "tool"
	}
	if len(s) > 32 {
		s = s[:32] // 截断只为文件系统上限，不改变语义
	}
	return s
}
