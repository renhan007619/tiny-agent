package interview

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// TagHit 是"这个标签出现过一次"的一条证据：什么时候、哪一场哪一题、当时几分。
type TagHit struct {
	At        time.Time
	SessionID string
	Index     int
	Tier      Tier
}

// TagTrend 是一个标签的全部出现记录，按时间从早到晚排好。
type TagTrend struct {
	Tag  string
	Hits []TagHit
}

// Direction 判断走势：拿最早一次和最近一次比。
//
// 为什么比首尾而不是比"上一场"：
// 上一场可能只是换了个场景没体现出来，拿单场比会把噪音当趋势。
// 首尾比看的是"从开始练到现在有没有挪窝"，这才是有意义的那个问题。
func (t TagTrend) Direction() string {
	switch {
	case len(t.Hits) == 0:
		return "无记录"
	case len(t.Hits) == 1:
		return "一次记录"
	}
	first := t.Hits[0].Tier
	last := t.Hits[len(t.Hits)-1].Tier
	switch {
	case last.Index() < first.Index():
		return "改善" // Index 越小档位越好
	case last.Index() > first.Index():
		return "恶化"
	default:
		return "持平"
	}
}

// Path 把档位序列压成一行给人看："E → C → B（改善）"。
// 只显示最近 5 次，再长的序列打印出来没人看。
func (t TagTrend) Path() string {
	if len(t.Hits) == 0 {
		return "无记录"
	}
	if len(t.Hits) == 1 {
		return string(t.Hits[0].Tier) + "（一次记录）"
	}

	hits := t.Hits
	prefix := ""
	if len(hits) > 5 {
		hits = hits[len(hits)-5:]
		prefix = "… → "
	}
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, string(h.Tier))
	}
	return prefix + strings.Join(parts, " → ") + "（" + t.Direction() + "）"
}

// LoadTagTrends 把全部场次的弱点标签摊平，按标签分组、按时间排好。
//
// 数据来源是 turn 表本身（weak 列里存着每题的弱点），不读 weak_point——
// 因为 weak_point 只留了「最近一次」，要看走势必须有完整历史，那只有 turn 有。
func (s *Store) LoadTagTrends() (map[string]TagTrend, error) {
	rows, err := s.db.Query(
		`SELECT session_id, idx, updated_at, tier, total, weak
		   FROM turn
		  WHERE weak <> '[]'
		  ORDER BY updated_at ASC, session_id ASC, idx ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]TagTrend)
	for rows.Next() {
		var (
			sid   string
			idx   int
			at    string
			tier  sql.NullString
			total sql.NullInt64
			weak  string
		)
		if err := rows.Scan(&sid, &idx, &at, &tier, &total, &weak); err != nil {
			return nil, err
		}

		// 先定这一题的档位：库里有就用库里的，老数据没有就用分数倒推。
		// 【必记】两样都没有（没打成分的题），直接跳过——
		// 拿一个编出来的档位去算走势，等于往趋势曲线上造假点。
		var tr Tier
		if tier.Valid {
			tr, _ = ParseTier(tier.String)
		}
		if tr == "" && total.Valid {
			tr = TierFromScore(int(total.Int64))
		}
		if tr == "" {
			continue
		}

		var wps []WeakPoint
		if err := json.Unmarshal([]byte(weak), &wps); err != nil {
			continue // 一条坏 JSON 不该毁掉整张走势图
		}

		ts := parseTime(at)
		for _, w := range wps {
			if w.Tag == "" {
				continue
			}
			t := out[w.Tag]
			t.Tag = w.Tag
			t.Hits = append(t.Hits, TagHit{At: ts, SessionID: sid, Index: idx, Tier: tr})
			out[w.Tag] = t
		}
	}
	return out, rows.Err()
}
