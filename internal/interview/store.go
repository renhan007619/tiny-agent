package interview

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite，不需要装 C 库
)

// Store 是一场场面试的记录本。
type Store struct {
	db *sql.DB
}

// OpenStore 打开（不存在则创建）数据库文件，并保证表都在。
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// WAL 模式：一个人写、另一个人读不会互相卡。
	// 这里就是"你正在答题、后台同时在打分"的场景。
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 WAL 失败: %w", err)
	}
	// 碰上别人正在写，最多等 5 秒再报错，而不是立刻失败。
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}

	// 四张表：场次 / 每道题 / 弱点累计（跨场次）/ 复习排期
	for _, q := range []string{schemaSession, schemaTurn, schemaWeakPoint, schemaReview} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("建表失败: %w", err)
		}
	}

	// 建完表还得补列：CREATE TABLE IF NOT EXISTS 只对"表不存在"生效，
	// 已经存在的表它一个字都不会改。所以老库升级必须单独走一遍。
	st := &Store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// migrate 给老库补上后来新增的列。
//
// 现在补 tier、hits（扣分制改造加的）和 grade_raw（重放改造加的）。
// 以后再加列，往下面这张表里加一行就行，别的都不用动。
func (s *Store) migrate() error {
	adds := []struct{ name, ddl string }{
		{"tier", `ALTER TABLE turn ADD COLUMN tier TEXT`},
		{"hits", `ALTER TABLE turn ADD COLUMN hits TEXT NOT NULL DEFAULT '[]'`},
		// grade_raw：模型当时返回的原文，原样存。
		//
		// 它是"回放"的唯一原料——没有它，改了解析逻辑只能重跑一场，
		// 而重跑的结果和你上次跑的不是同一份，比了也说明不了问题。
		//
		// 为什么现在就要加、不能等有空再补：这一列从加上的那一刻才开始攒，
		// 加得越晚，能回放的存量越少。
		{"grade_raw", `ALTER TABLE turn ADD COLUMN grade_raw TEXT NOT NULL DEFAULT ''`},
		// L2 事件层两列。
		//
		// depth_reached 故意不加 NOT NULL DEFAULT 0：
		// 0 是"答了但没判出层次"，是真数据；"没打成"必须是 NULL。
		// 把没打成写成 0，等于往库里塞一条"这人一层都没讲到"的假事实。
		{"depth_reached", `ALTER TABLE turn ADD COLUMN depth_reached INTEGER`},
		{"suspect", `ALTER TABLE turn ADD COLUMN suspect TEXT NOT NULL DEFAULT ''`},
	}
	for _, a := range adds {
		if s.hasColumn("turn", a.name) {
			continue // 新库建表时已经有了
		}
		if _, err := s.db.Exec(a.ddl); err != nil {
			return fmt.Errorf("给 turn 补列 %s 失败: %w", a.name, err)
		}
	}
	return nil
}

// hasColumn 查一张表有没有某一列。
//
// 用 PRAGMA table_info 而不是靠 ALTER 的错误码：
// 拿到列清单能看清楚到底有什么，将来要查别的列也用得上。
func (s *Store) hasColumn(table, col string) bool {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == col {
			return true
		}
	}
	return false
}

const schemaSession = `CREATE TABLE IF NOT EXISTS session (
	id         TEXT PRIMARY KEY,
	started_at TEXT NOT NULL,
	ended_at   TEXT,
	status     TEXT NOT NULL DEFAULT 'ongoing'
)`

// 分数列故意不加 NOT NULL DEFAULT 0：
// 没打分的时候要能写 NULL。0 分是有效数据，会进复习排期；
// 把"没打成"写成 0 分，等于往库里塞一条假事实。
//
// tier / hits 是扣分制改造时加的：
//
//	tier  档位 A-E，由代码算，不由模型给
//	hits  命中的扣分项 id 列表，JSON 数组
//
// depth/project/talk/solve 四个旧列留在表里但不再写入——
// 老数据（改造前那几场）还在用它们，删列会让老记录读不出来。
const schemaTurn = `CREATE TABLE IF NOT EXISTS turn (
	session_id TEXT    NOT NULL,
	idx        INTEGER NOT NULL,
	stage      INTEGER NOT NULL,
	question   TEXT    NOT NULL,
	answer     TEXT    NOT NULL DEFAULT '',
	grade      TEXT    NOT NULL DEFAULT 'pending',
	total      INTEGER,
	tier       TEXT,
	hits       TEXT    NOT NULL DEFAULT '[]',
	depth      INTEGER,
	project    INTEGER,
	talk       INTEGER,
	solve      INTEGER,
	partial    INTEGER NOT NULL DEFAULT 0,
	comment    TEXT    NOT NULL DEFAULT '',
	weak       TEXT    NOT NULL DEFAULT '[]',
	grade_err  TEXT    NOT NULL DEFAULT '',
	grade_raw  TEXT    NOT NULL DEFAULT '',
	depth_reached INTEGER,
	suspect    TEXT    NOT NULL DEFAULT '',
	updated_at TEXT    NOT NULL,
	PRIMARY KEY (session_id, idx)
)`

// weak_point 是"跨场次"的账本：一个标签只有一行，错了就把次数 +1。
//
// 它跟 turn 表的分工：
//
//	turn       = 这一场发生了什么（一场一份，越积越多）
//	weak_point = 我这个人到底有哪些稳定毛病（一个人一份，只会变准不会变多）
//
// 表里只存"最细的那一格"（比如 并发/channel 关闭与阻塞），不另存父级的合计数字。
// 父级的次数是出报告时现场加出来的——同一个事实只存一份，改一处不会漏一处。
//
// last_raw_tag 存模型原本想写的名字（可能比库里的更细），
// 这样"退到父级"的时候，细节还留在库里，不会真的丢掉。
const schemaWeakPoint = `CREATE TABLE IF NOT EXISTS weak_point (
	tag          TEXT PRIMARY KEY,
	count        INTEGER NOT NULL DEFAULT 0,
	last_detail  TEXT    NOT NULL DEFAULT '',
	last_raw_tag TEXT    NOT NULL DEFAULT '',
	last_session TEXT    NOT NULL DEFAULT '',
	last_idx     INTEGER NOT NULL DEFAULT 0,
	first_seen   TEXT    NOT NULL,
	last_seen    TEXT    NOT NULL
)`

// CreateSession 记下一场面试的开始。
func (s *Store) CreateSession(id string, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO session (id, started_at, status) VALUES (?, ?, 'ongoing')`,
		id, at.Format(time.RFC3339))
	return err
}

// EndSession 记下一场面试的结束。
func (s *Store) EndSession(id string, at time.Time) error {
	_, err := s.db.Exec(
		`UPDATE session SET ended_at = ?, status = 'finished' WHERE id = ?`,
		at.Format(time.RFC3339), id)
	return err
}

// SaveQuestion 把刚问出来的题存下来。
func (s *Store) SaveQuestion(sessionID string, t Turn) error {
	_, err := s.db.Exec(
		`INSERT INTO turn (session_id, idx, stage, question, grade, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, t.Index, int(t.Stage), t.Question, string(t.Grade), nowText())
	return err
}

// SaveAnswer 把候选人这一题的回答补上去。
func (s *Store) SaveAnswer(sessionID string, idx int, answer string) error {
	_, err := s.db.Exec(
		`UPDATE turn SET answer = ?, updated_at = ? WHERE session_id = ? AND idx = ?`,
		answer, nowText(), sessionID, idx)
	return err
}

// SaveGrade 记下一次判卷的结果，或者是"没打成"的原因。
//
// 这里就是"失败不许伪装成 0 分"的落地点：
// 分数列宁可留 NULL，也要把 grade 和 grade_err 写明白。
//
// 注意 depth/project/talk/solve 四列不再写入——它们属于旧的"四维自由打分"，
// 新数据的这四列是 NULL。老数据那几场仍然读得出来，因为列还在。
// raw 是模型这次的原始返回，原样存下来给回放用。
// 没调模型的场合（反问、空答案）传空字符串——这一列是 NOT NULL，
// 不能像分数那样用 any 传 nil。
func (s *Store) SaveGrade(sessionID string, idx int, g GradeState, sc *Score, rv *Review, errText, raw string) error {
	// 用 any 是为了没分数时能真的写 NULL，而不是写 0。
	var total, tier, depth any
	hits := "[]"
	partial := 0
	suspect := ""

	if sc != nil {
		total = sc.Total
		if sc.Tier != "" {
			tier = string(sc.Tier)
		}
		if b, err := json.Marshal(sc.Hits); err == nil {
			hits = string(b)
		}
		if sc.Partial {
			partial = 1
		}
		// depth 只在真判过时才有值。sc 为 nil（没打成）时它保持 nil → 写 NULL，
		// 和"判了但一层都没讲到（0）"分得清清楚楚。
		// 这就是 store.go 开头那条规矩：失败不许伪装成 0 分。
		depth = sc.Depth
		suspect = sc.Suspect
	}

	comment, weak := "", "[]"
	if rv != nil {
		comment = rv.Comment
		if b, err := json.Marshal(rv.WeakPoints); err == nil {
			weak = string(b)
		}
		if rv.Partial {
			partial = 1
		}
	}

	_, err := s.db.Exec(
		`UPDATE turn SET grade = ?, total = ?, tier = ?, hits = ?,
		        partial = ?, comment = ?, weak = ?, grade_err = ?, grade_raw = ?,
		        depth_reached = ?, suspect = ?, updated_at = ?
		 WHERE session_id = ? AND idx = ?`,
		string(g), total, tier, hits,
		partial, comment, weak, errText, raw,
		depth, suspect, nowText(), sessionID, idx)
	return err
}

// UpsertWeakPoint 记一次弱点。
//
// 同一个 tag 只有一行：没有就插一条，已经有就次数 +1、更新"最近细节"。
// 这就是"以标签为唯一键"，也是整个弱点统计能成立的原因——
// 以前一句话一条的存法，同一个毛病换个说法就是两行，永远数不清。
//
// rawTag 是模型原本写的名字，原样存着；tag 是 Resolve 之后、库里一定有的名字。
func (s *Store) UpsertWeakPoint(tag, detail, rawTag, sessionID string, idx int) error {
	now := nowText()
	_, err := s.db.Exec(
		`INSERT INTO weak_point (tag, count, last_detail, last_raw_tag, last_session, last_idx, first_seen, last_seen)
		 VALUES (?, 1, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tag) DO UPDATE SET
		   count        = count + 1,
		   last_detail  = excluded.last_detail,
		   last_raw_tag = excluded.last_raw_tag,
		   last_session = excluded.last_session,
		   last_idx     = excluded.last_idx,
		   last_seen    = excluded.last_seen`,
		tag, detail, rawTag, sessionID, idx, now, now)
	if err != nil {
		return err
	}

	// 新标签顺手挂上复习排期；已经排过期的保持原样，练习进度不能被冲掉。
	//
	// 这里不包事务：万一账本记下了、排期没挂上，下次再犯同一个毛病会自动补上
	// ——因为 StartReview 是幂等的（DO NOTHING）。
	// 为一个能自愈的状态去加事务，属于把简单问题复杂化。
	if err := s.StartReview(tag, time.Now()); err != nil {
		return err
	}
	return nil
}

// WeakStat 是一个标签的累计情况。
type WeakStat struct {
	Tag         string
	Count       int
	LastDetail  string // 最近一次具体怎么错的
	LastRawTag  string // 最近一次模型原本想写的名字
	LastSession string // 最近一次是哪一场
	LastIdx     int    // 最近一次是第几题
	LastSeen    string
}

// LoadWeakPoints 拿全部弱点，错得多的排前面。
func (s *Store) LoadWeakPoints() ([]WeakStat, error) {
	rows, err := s.db.Query(
		`SELECT tag, count, last_detail, last_raw_tag, last_session, last_idx, last_seen
		   FROM weak_point ORDER BY count DESC, tag ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WeakStat
	for rows.Next() {
		var w WeakStat
		if err := rows.Scan(&w.Tag, &w.Count, &w.LastDetail, &w.LastRawTag,
			&w.LastSession, &w.LastIdx, &w.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// LoadTurns 把一场面试的题全部读回来，按题号排好。
// 报告用它，以后恢复面试也用它——看到的就是库里真实存着的。
func (s *Store) LoadTurns(sessionID string) ([]Turn, error) {
	rows, err := s.db.Query(
		`SELECT idx, stage, question, answer, grade,
		        total, tier, hits,
		        partial, comment, weak, grade_err,
		        depth_reached, suspect
		   FROM turn WHERE session_id = ? ORDER BY idx`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var (
			t                       Turn
			stage                   int
			total                   sql.NullInt64
			tier, hits              sql.NullString
			partial                 int
			comment, weak, gradeErr string
			depthReached            sql.NullInt64
			suspect                 string
		)
		if err := rows.Scan(&t.Index, &stage, &t.Question, &t.Answer, &t.Grade,
			&total, &tier, &hits,
			&partial, &comment, &weak, &gradeErr,
			&depthReached, &suspect); err != nil {
			return nil, err
		}
		t.Stage = Stage(stage)
		t.GradeErr = gradeErr

		if total.Valid {
			sc := &Score{
				Total:    int(total.Int64),
				Partial:  partial == 1,
				Hits:     []string{},
				Evidence: map[string]string{},
			}

			// 档位优先读库里的；老数据（改造前那几场）没有这一列，
			// 就用分数换算一个出来，报告里才不会一半有档位一半没有。
			if tier.Valid {
				if tv, ok := ParseTier(tier.String); ok {
					sc.Tier = tv
				}
			}
			if sc.Tier == "" {
				sc.Tier = TierFromScore(sc.Total)
			}

			// 同上：老数据没有 hits 这一列，留空切片而不是 nil，
			// 这样报告里写"这条回答没踩坑"只会出现在真的零命中的新数据上。
			if hits.Valid && hits.String != "" {
				if err := json.Unmarshal([]byte(hits.String), &sc.Hits); err != nil {
					sc.Hits = []string{}
				}
			}
			if sc.Hits == nil {
				sc.Hits = []string{}
			}

			// L2：老数据这两列是 NULL / 空串，读出来显示成"未判定"。
			if depthReached.Valid {
				sc.Depth = int(depthReached.Int64)
			}
			sc.Suspect = suspect

			t.Score = sc
		}
		if comment != "" || weak != "[]" {
			t.Review = &Review{Comment: comment, Partial: partial == 1}
			if err := json.Unmarshal([]byte(weak), &t.Review.WeakPoints); err != nil || t.Review.WeakPoints == nil {
				t.Review.WeakPoints = []WeakPoint{}
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OpenSession 是一场"开了但没结束"的面试，带着能不能续的判断依据。
//
// Turns / Answered 是给人看的，不是给代码下判断用的：
// 「已答 0 题」几乎一定是一次误触，这比任何时间阈值都准，也不会过期。
type OpenSession struct {
	ID         string
	StartedAt  time.Time
	LastActive time.Time // 最后一次往这场里写字的时间（答题或打分都算）
	Turns      int       // 问了几题
	Answered   int       // 答了几题
}

// FindOpenSessions 找出所有"有内容但没结束"的面试，最近动过的排前面。
//
// 为什么不是只取最后一条：最后一条可能是一次误触（开了场立刻退），
// 而真正面到一半的是倒数第二条。所以不猜，全列出来交给用户挑。
//
// 为什么按 last_active 排而不按 started_at：人一次只面一场，
// 想续的几乎总是最近动过的那场；只按开始时间排，遇到"开了一场挂在旁边很久"
// 就会选错。
//
// HAVING answered > 0 是唯一一条硬过滤：一题都没答过的场次没有续的价值。
// 它必须写在 HAVING 而不是 WHERE——它是聚合之后才能算的条件。
func (s *Store) FindOpenSessions() ([]OpenSession, error) {
	rows, err := s.db.Query(
		`SELECT s.id, s.started_at,
		        COALESCE(MAX(t.updated_at), s.started_at) AS last_active,
		        COUNT(t.idx) AS turns,
		        SUM(CASE WHEN t.answer <> '' THEN 1 ELSE 0 END) AS answered
		   FROM session s
		   LEFT JOIN turn t ON t.session_id = s.id
		  WHERE s.status = 'ongoing'
		  GROUP BY s.id, s.started_at
		 HAVING answered > 0
		  ORDER BY last_active DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OpenSession
	for rows.Next() {
		var (
			o                   OpenSession
			started, lastActive string
			answered            sql.NullInt64 // LEFT JOIN 没配上 turn 时 SUM 是 NULL
		)
		if err := rows.Scan(&o.ID, &started, &lastActive, &o.Turns, &answered); err != nil {
			return nil, err
		}
		o.StartedAt = parseTime(started)
		o.LastActive = parseTime(lastActive)
		o.Answered = int(answered.Int64)
		out = append(out, o)
	}
	return out, rows.Err()
}

// LoadUngradedTurns 找出这一场里"欠着一次打分"的题。
//
// 判据只有两条：答过（answer 不为空）、分数没有终局（grade 不是 done/skipped）。
//
// 为什么 running 也算欠：进程被打断时那题停在 running，
// 它的分其实没算完。不重打，报告里会永远显示"打分中"。
//
// 为什么用参数传常量而不在 SQL 里写死 'done'/'skipped'：
// 常量将来改名，这里跟着变，不会偷偷漏掉一类题。
func (s *Store) LoadUngradedTurns(sessionID string) ([]Turn, error) {
	rows, err := s.db.Query(
		`SELECT idx, stage, question, answer, grade, grade_err
		   FROM turn
		  WHERE session_id = ?
		    AND answer <> ''
		    AND grade NOT IN (?, ?)
		  ORDER BY idx`,
		sessionID, string(GradeDone), string(GradeSkipped))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var (
			t     Turn
			stage int // Stage 是 int 的别名，Scan 不认，得先接成 int 再转
		)
		if err := rows.Scan(&t.Index, &stage, &t.Question, &t.Answer,
			&t.Grade, &t.GradeErr); err != nil {
			return nil, err
		}
		t.Stage = Stage(stage)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RawGrade 是一条"有原始判词"的记录，回放用。
//
// 它把三样东西凑在一起：
//
//	Raw    —— 模型当时说了什么（证据，唯一不可重建的东西）
//	Answer —— 候选人的回答（代码预检 CheckByCode 要用；没有它就没法重算 hits）
//	Old... —— 当时算出来的结论（对照的基线）
type RawGrade struct {
	SessionID string
	Index     int
	Question  string
	Answer    string
	Raw       string

	OldTier  Tier
	OldTotal int
	// OldTotalValid 说明"当时真的存过分"。
	//
	// 为什么必须有它：total 这列允许 NULL（没打成就是 NULL），
	// 而读出来之后 int 是 0。不分清的话，"没存"会被当成"考了 0 分"，
	// 平均分基线被拉低，算出来的涨跌幅虚高——存量老数据越多虚得越厉害。
	OldTotalValid bool
	OldHits       []string
}

// LoadRawGrades 读出所有能回放的题。sessionID 传空字符串表示全部场次。
//
// 为什么只挑「grade_raw 不为空」的：改造之前的数据没有原文，回放不了。
// 这不是 bug，是"先存证据才有回放"的必然结果——所以这一列越早加越好，
// 它从今天起才开始攒。
func (s *Store) LoadRawGrades(sessionID string) ([]RawGrade, error) {
	q := `SELECT session_id, idx, question, answer, grade_raw, tier, total, hits
	        FROM turn
	       WHERE grade_raw <> ''`
	args := []any{}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY session_id, idx`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RawGrade
	for rows.Next() {
		var (
			r     RawGrade
			tier  sql.NullString
			total sql.NullInt64
			hits  string
		)
		if err := rows.Scan(&r.SessionID, &r.Index, &r.Question, &r.Answer,
			&r.Raw, &tier, &total, &hits); err != nil {
			return nil, err
		}

		// total 是"可以没有"的列，所以用 NullInt64 读，
		// 并且把 Valid 一起留下来——它决定了这条能不能进平均分统计。
		r.OldTotalValid = total.Valid
		if total.Valid {
			r.OldTotal = int(total.Int64)
		}

		// 老数据没有 tier 列，用分数换算一个出来，保证新旧能对上。
		if tier.Valid {
			if t, ok := ParseTier(tier.String); ok {
				r.OldTier = t
			}
		}
		// 注意顺序：只有"当时真的存过分"才允许用分数倒推档位。
		// total 是 NULL 时倒推会得到 0 分 → E 档，那是凭空造出来的基线，
		// 会让回放报出一堆假的"档位变化"。
		if r.OldTier == "" && r.OldTotalValid {
			r.OldTier = TierFromScore(r.OldTotal)
		}

		// hits 解析失败就当空，不让一条坏 JSON 挡住整批回放。
		if err := json.Unmarshal([]byte(hits), &r.OldHits); err != nil {
			r.OldHits = nil
		}

		out = append(out, r)
	}
	return out, rows.Err()
}

// parseTime 解析库里存的 RFC3339 文本，坏了就退回"现在"。
// 时间只是显示用的信息，不该因为一个坏字符串把整场面试拦在门外。
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return t
}

// SessionBrief 是一场面试的小档案，全量体检用它遍历。
type SessionBrief struct {
	ID        string
	StartedAt time.Time
	Finished  bool
	Turns     int
}

// ListSessions 列出所有场次，最近开的排前面。
//
// 题数用相关子查询现算，不落成字段：它和 turn 表是同一个事实的两种看法，
// 存两处迟早对不上。
func (s *Store) ListSessions() ([]SessionBrief, error) {
	rows, err := s.db.Query(
		`SELECT s.id, s.started_at, s.status,
		        (SELECT COUNT(*) FROM turn t WHERE t.session_id = s.id)
		   FROM session s
		  ORDER BY s.started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionBrief
	for rows.Next() {
		var (
			b    SessionBrief
			at   string
			stat string
		)
		if err := rows.Scan(&b.ID, &at, &stat, &b.Turns); err != nil {
			return nil, err
		}
		b.StartedAt = parseTime(at)
		b.Finished = stat == "finished"
		out = append(out, b)
	}
	return out, rows.Err()
}

// Close 关掉数据库。
func (s *Store) Close() error {
	return s.db.Close()
}

func nowText() string {
	return time.Now().Format(time.RFC3339)
}
