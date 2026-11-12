package interview

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestStore 开一个临时库。每个用例一个文件，互不干扰。
//
// 用 t.TempDir() 而不是 ":memory:"：Go 的 sql.DB 是连接池，
// 内存库在每个连接里是独立的一份，池里换条连接就查不到刚才写的表了——
// 那是新手最容易踩的一个坑。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("开临时库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 固定一个时间基准，避免测试跑在半夜的时候结果飘。
func testNow() time.Time {
	return time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)
}

// 挂了排期，今天就能在"该练了"里看到。
func TestStartReview_ShowsUpDue(t *testing.T) {
	s := newTestStore(t)
	now := testNow()

	if err := s.StartReview("并发/channel 关闭与阻塞", now); err != nil {
		t.Fatalf("挂排期失败: %v", err)
	}
	due, err := s.DueReviews(now)
	if err != nil {
		t.Fatalf("查到期待练失败: %v", err)
	}
	if len(due) != 1 || due[0].Tag != "并发/channel 关闭与阻塞" {
		t.Fatalf("应该查到刚挂的那条，实际: %+v", due)
	}
	if due[0].Box != 0 {
		t.Fatalf("刚挂上应该是第 0 档，实际 %d", due[0].Box)
	}
}

// 【必记】又有新证据时不能冲掉练习进度。
// 这条是"新账不能把旧账推平"的守门人：do nothing 一改成 do update，这条就红。
func TestStartReview_DoesNotResetProgress(t *testing.T) {
	s := newTestStore(t)
	now := testNow()

	if err := s.StartReview("标签", now); err != nil {
		t.Fatal(err)
	}
	// 练一次，进到第 1 档
	if _, err := s.MarkReviewed("标签", ResultGood, now); err != nil {
		t.Fatal(err)
	}
	// 又发现一次这个毛病——不该把档位打回 0
	if err := s.StartReview("标签", now); err != nil {
		t.Fatal(err)
	}

	var box int
	if err := s.db.QueryRow(`SELECT box FROM review WHERE tag = ?`, "标签").Scan(&box); err != nil {
		t.Fatal(err)
	}
	if box != 1 {
		t.Fatalf("练习进度被重置了：应该是第 1 档，实际 %d", box)
	}
}

// 三档调度：又忘了退、还行退一格进、很熟进两档。
func TestMarkReviewed_ThreeAnswers(t *testing.T) {
	cases := []struct {
		name     string
		startBox int
		result   ReviewResult
		wantBox  int
		wantDays int
	}{
		{"第0档+又忘了→还是0，明天再来", 0, ResultAgain, 0, 1},
		{"第0档+还行→1档，3天后", 0, ResultGood, 1, 3},
		{"第0档+很熟→2档，7天后", 0, ResultEasy, 2, 7},
		{"第3档+很熟→封顶到4档，30天后", 3, ResultEasy, 4, 30},
		{"第4档+很熟→不越界，还是30天后", 4, ResultEasy, 4, 30},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			now := testNow()

			if err := s.StartReview("标签", now); err != nil {
				t.Fatal(err)
			}
			// 直接改 box 到起始档位，省去一路练上去
			if _, err := s.db.Exec(`UPDATE review SET box = ? WHERE tag = ?`, c.startBox, "标签"); err != nil {
				t.Fatal(err)
			}

			next, err := s.MarkReviewed("标签", c.result, now)
			if err != nil {
				t.Fatalf("记结果失败: %v", err)
			}
			if got := dateOnly(next); got != dateOnly(now.Add(time.Duration(c.wantDays)*24*time.Hour)) {
				t.Fatalf("下次日期不对：想要 %s，实际 %s",
					dateOnly(now.Add(time.Duration(c.wantDays)*24*time.Hour)), got)
			}

			var box int
			if err := s.db.QueryRow(`SELECT box FROM review WHERE tag = ?`, "标签").Scan(&box); err != nil {
				t.Fatal(err)
			}
			if box != c.wantBox {
				t.Fatalf("档位不对：想要 %d，实际 %d", c.wantBox, box)
			}
		})
	}
}

// 没这个排期时记结果，不能报错也不能凭空建一条。
func TestMarkReviewed_NoSchedule(t *testing.T) {
	s := newTestStore(t)

	next, err := s.MarkReviewed("从没挂过的标签", ResultGood, testNow())
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !next.IsZero() {
		t.Fatalf("没排期应该返回零值时间，实际 %v", next)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM review`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不该凭空建排期，实际建了 %d 条", n)
	}
}

// 只拿今天到期的，未来的不能提前冒出来。
func TestDueReviews_OnlyDueOnes(t *testing.T) {
	s := newTestStore(t)
	now := testNow()

	if err := s.StartReview("今天该练", now); err != nil {
		t.Fatal(err)
	}
	if err := s.StartReview("明天再练", now); err != nil {
		t.Fatal(err)
	}
	// 把第二条推到明天
	if _, err := s.db.Exec(`UPDATE review SET next_review = ? WHERE tag = ?`,
		dateOnly(now.Add(24*time.Hour)), "明天再练"); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueReviews(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Tag != "今天该练" {
		t.Fatalf("只该查到今天到期的，实际: %+v", due)
	}
}

// 到期条目要带上弱点账本里的"犯过几次、上次错在哪"。
// 只给排期你不知道练什么，只给账本你不知道今天该练哪个。
func TestDueReviews_CarriesWeakDetail(t *testing.T) {
	s := newTestStore(t)
	now := testNow()

	// UpsertWeakPoint 现在会自动挂排期，时间由调用方传（不能读真实时钟）
	if err := s.UpsertWeakPoint("并发/channel 关闭与阻塞", "只说了要 close，没说谁 close",
		"并发/channel 关闭与阻塞", "sess-1", 3, now); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueReviews(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("应该有一条到期，实际 %d 条", len(due))
	}
	d := due[0]
	if d.Count != 1 {
		t.Fatalf("次数应该带出来，实际 %d", d.Count)
	}
	if d.LastDetail == "" {
		t.Fatal("上次的细节应该带出来")
	}
	if d.LastSession != "sess-1" || d.LastIdx != 3 {
		t.Fatalf("来源场次/题号没带对: %s/%d", d.LastSession, d.LastIdx)
	}
}

// 回填：把"账本里有、但没排期"的补上，已有的不碰。
func TestBackfillReviews_OnlyMissing(t *testing.T) {
	s := newTestStore(t)
	now := testNow()

	// 手工塞两条"老数据"的弱点记录（绕过 UpsertWeakPoint，它现在会自动挂排期）。
	// 这模拟的就是改造之前攒下来的那些账。
	rawWeak := func(tag string) {
		t.Helper()
		_, err := s.db.Exec(
			`INSERT INTO weak_point (tag, count, last_detail, last_raw_tag, last_session, last_idx, first_seen, last_seen)
			 VALUES (?, 2, '老细节', ?, 'old-sess', 1, ?, ?)`,
			tag, tag, dateOnly(now.AddDate(0, 0, -10)), dateOnly(now.AddDate(0, 0, -10)))
		if err != nil {
			t.Fatalf("塞老弱点失败: %v", err)
		}
	}
	rawWeak("老毛病A")
	rawWeak("老毛病B")

	// 其中一条已经有排期了（而且是在未来），不该被动
	if err := s.StartReview("老毛病B", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE review SET box = 3, next_review = ? WHERE tag = ?`,
		dateOnly(now.AddDate(0, 0, 15)), "老毛病B"); err != nil {
		t.Fatal(err)
	}

	n, err := s.BackfillReviews(now)
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("只该补 1 条（老毛病A），实际补了 %d 条", n)
	}

	// 补的那条：今天到期、第 0 档（历史欠账从今天开始还）
	due, err := s.DueReviews(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Tag != "老毛病A" {
		t.Fatalf("回填的应该是老毛病A 且今天到期，实际: %+v", due)
	}

	// 已有排期的那条不能被改回第 0 档
	var box int
	var next string
	if err := s.db.QueryRow(`SELECT box, next_review FROM review WHERE tag = ?`, "老毛病B").Scan(&box, &next); err != nil {
		t.Fatal(err)
	}
	if box != 3 {
		t.Fatalf("已有的排期被回填改动了：档位应该是 3，实际 %d", box)
	}
	if next != dateOnly(now.AddDate(0, 0, 15)) {
		t.Fatalf("已有的下次日期被改动了：实际 %s", next)
	}
}

// 【必记】排期存的是日期字符串，不是完整时间戳。
// 这条守住的是时区坑：带时区的时间在 SQL 里比较，SQLite 会先转 UTC，
// 东八区早上 8 点前会被算成前一天。
func TestReviewDate_NoTimezoneDrift(t *testing.T) {
	// 凌晨 1 点：这个点在某些时区换算里最容易"变成昨天"
	early := time.Date(2026, 11, 9, 1, 0, 0, 0, time.Local)
	if got := dateOnly(early); got != "2026-11-09" {
		t.Fatalf("凌晨 1 点应该还是当天，实际 %s", got)
	}

	s := newTestStore(t)
	if err := s.StartReview("标签", early); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := s.db.QueryRow(`SELECT next_review FROM review WHERE tag = ?`, "标签").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "2026-11-09" {
		t.Fatalf("库里存的应该是 2026-11-09，实际 %s", stored)
	}
	// 而且这一天必须查得到（不能因为时区换算跑到昨天去）
	due, err := s.DueReviews(early)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("凌晨也该查得到今天到期的，实际 %d 条", len(due))
	}
}
