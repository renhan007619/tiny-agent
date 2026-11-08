package interview

import (
	"database/sql"
	"time"
)

// schemaReview 是复习排期表：一个标签一行。
//
// 为什么不挂在 weak_point 表上加列：
//
//	weak_point 是「病历汇总」，记的是你身上有哪些毛病——历史事实，不能因为换算法就重置。
//	review     是「下次复诊预约」，记的是练习进度——随时可以删掉重灌。
//
// 两样东西的生命周期不一样，混在一张表里，将来想换调度算法或换成 Redis，
// 就得动病历本本身。
const schemaReview = `CREATE TABLE IF NOT EXISTS review (
	tag         TEXT PRIMARY KEY,
	box         INTEGER NOT NULL DEFAULT 0,
	next_review TEXT    NOT NULL,
	last_result TEXT    NOT NULL DEFAULT '',
	last_review TEXT    NOT NULL DEFAULT ''
)`

// reviewSteps 是每一档的间隔天数：第 0 档明天，第 4 档一个月后。
//
// 为什么只有 5 档、为什么不照抄完整的 SM-2：
// 这个工具一天只花你一分钟，档位越少你越敢用；参数越多你越会去调参数，
// 最后调参数的时间比练的时间长。
var reviewSteps = []int{1, 3, 7, 15, 30}

// ReviewResult 是你练完之后给自己的判断。只有三档，一眼能选。
type ReviewResult string

const (
	ResultAgain ReviewResult = "again" // 又忘了：退一档，明天再来
	ResultGood  ReviewResult = "good"  // 还行：进一档
	ResultEasy  ReviewResult = "easy"  // 很熟：进两档，让它久一点再出现
)

// nextBox 算出练完之后进哪一档。
//
// 退档退到底就停在 0（不能变成负数，否则取间隔数组会越界）；
// 进档进到头就停在最后一档（30 天封顶，不然熟的东西永远不再出现）。
func nextBox(box int, r ReviewResult) int {
	switch r {
	case ResultAgain:
		box--
	case ResultEasy:
		box += 2
	default:
		box++
	}
	if box < 0 {
		box = 0
	}
	if box >= len(reviewSteps) {
		box = len(reviewSteps) - 1
	}
	return box
}

// boxInterval 是这一档隔几天。
func boxInterval(box int) time.Duration {
	if box < 0 {
		box = 0
	}
	if box >= len(reviewSteps) {
		box = len(reviewSteps) - 1
	}
	return time.Duration(reviewSteps[box]) * 24 * time.Hour
}

// dateOnly 把时间压成 "2006-01-02"。
//
// 【必记】排期为什么存日期而不是完整时间戳：
// 复习是「天」级别的动作，存到秒没有意义。更要紧的是——用带时区的完整时间存，
// 在 SQL 里比较就得小心换算（SQLite 的 date() 会先转成 UTC，
// 东八区早上 8 点之前会被算成前一天）。存纯日期、按字符串比较，
// 格式固定所以字典序就是时间序，一个坑都不踩。
func dateOnly(t time.Time) string {
	return t.Format("2006-01-02")
}

// StartReview 给一个标签挂上复习排期。已经有排期的不动。
//
// 为什么是 DO NOTHING 而不是覆盖：排期是你的练习进度，
// 不能被"又发现一次这个毛病"冲掉。旧账只能靠练去推，不能被新证据重置。
func (s *Store) StartReview(tag string, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO review (tag, box, next_review, last_result, last_review)
		 VALUES (?, 0, ?, '', '')
		 ON CONFLICT(tag) DO NOTHING`,
		tag, dateOnly(at))
	return err
}

// BackfillReviews 把「弱点账本里有、但还没排期」的标签一次性补进来。
//
// 为什么需要它：这套东西是今天才加的，你之前那几场面试攒下的弱点
// 一条排期都没有。不回填的话，第一次跑 review 会显示"今天没有要练的"——
// 明明一身毛病，工具却说没事，这比没有工具更糟。
//
// 回填成"今天到期"，意思就是：这些历史欠账，从今天开始还。
func (s *Store) BackfillReviews(now time.Time) (int, error) {
	res, err := s.db.Exec(
		`INSERT INTO review (tag, box, next_review, last_result, last_review)
		 SELECT w.tag, 0, ?, '', ''
		   FROM weak_point w
		  WHERE NOT EXISTS (SELECT 1 FROM review r WHERE r.tag = w.tag)`,
		dateOnly(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DueReview 是今天该练的一条。
//
// 它把排期和弱点账本拼在一起：排期说"该练了"，账本说"这毛病犯了 4 次、
// 上次具体错在哪"。只给排期你不知道练什么，只给账本你不知道今天该练哪个。
type DueReview struct {
	Tag         string
	Box         int
	NextReview  time.Time
	Count       int    // 犯过几次
	LastDetail  string // 上次具体差在哪
	LastSession string // 上次是哪一场
	LastIdx     int    // 上次是第几题
}

// DueReviews 拿出今天到期的条目，欠得最久的排前面。
func (s *Store) DueReviews(now time.Time) ([]DueReview, error) {
	rows, err := s.db.Query(
		`SELECT r.tag, r.box, r.next_review,
		        COALESCE(w.count, 0), COALESCE(w.last_detail, ''),
		        COALESCE(w.last_session, ''), COALESCE(w.last_idx, 0)
		   FROM review r
		   LEFT JOIN weak_point w ON w.tag = r.tag
		  WHERE r.next_review <= ?
		  ORDER BY r.next_review ASC, r.box ASC`,
		dateOnly(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DueReview
	for rows.Next() {
		var (
			d  DueReview
			at string
		)
		if err := rows.Scan(&d.Tag, &d.Box, &at,
			&d.Count, &d.LastDetail, &d.LastSession, &d.LastIdx); err != nil {
			return nil, err
		}
		d.NextReview, _ = time.Parse("2006-01-02", at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkReviewed 记一次练习结果，算出下次什么时候再来。返回下次的日期供打印。
//
// 先查 box 再更新，不是一条 SQL 里算完：
// "进几档"是 Go 里的规则（nextBox），SQL 只负责存。
// 规则留在代码里、数据留给 SQL，将来调档位不用动 SQL。
func (s *Store) MarkReviewed(tag string, r ReviewResult, at time.Time) (time.Time, error) {
	var box int
	if err := s.db.QueryRow(`SELECT box FROM review WHERE tag = ?`, tag).Scan(&box); err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, nil // 没这个排期，什么也不做
		}
		return time.Time{}, err
	}

	nb := nextBox(box, r)
	next := at.Add(boxInterval(nb))

	_, err := s.db.Exec(
		`UPDATE review SET box = ?, next_review = ?, last_result = ?, last_review = ?
		  WHERE tag = ?`,
		nb, dateOnly(next), string(r), dateOnly(at), tag)
	return next, err
}
