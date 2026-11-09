package interview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustTagBook 写一份临时标签文件再读进来。
//
// 故意走 LoadTags 而不是手搓 TagBook 结构体：
// 手搓的话，byPath 和 parents 两张表是我自己填的，
// 万一 LoadTags 里算父级的逻辑写错了，测试照样全绿——那就白测了。
func mustTagBook(t *testing.T, lines ...string) *TagBook {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tags.txt")
	body := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时标签库失败: %v", err)
	}
	b, err := LoadTags(path)
	if err != nil {
		t.Fatalf("读临时标签库失败: %v", err)
	}
	return b
}

// Resolve 的三级回退是整个记忆系统的地基：标签对不上，弱点统计全是废的。
func TestResolve_ThreeLevels(t *testing.T) {
	book := mustTagBook(t,
		"并发",
		"并发/channel 关闭与阻塞",
		"并发/GMP 调度",
		"表达",
		"表达/只讲做了什么没讲为什么",
	)

	cases := []struct {
		name string
		in   string
		want string
	}{
		// ① 精确命中，且是最细一级
		{"精确命中细项", "并发/channel 关闭与阻塞", "并发/channel 关闭与阻塞"},

		// ② 逐级往上砍，砍到库里有的细项
		{"多写了一级，砍到细项", "并发/channel 关闭与阻塞/具体细节", "并发/channel 关闭与阻塞"},

		// ② 砍到的是父级 → 记到"未细分"格子
		{"子项不存在，退到父级", "并发/完全不存在的子项", "并发/未细分"},

		// ③ 模型只报了父级（这一步最容易漏，第 147 行那段就为它写的）
		{"只报父级", "并发", "并发/未细分"},

		// ④ 完全对不上
		{"库里没有这组", "火星语/陨石坑", "未归类"},

		// 空白
		{"首尾有空格", "   并发   ", "并发/未细分"},
		{"纯空白", "   ", "未归类"},
		{"空字符串", "", "未归类"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := book.Resolve(c.in); got != c.want {
				t.Fatalf("Resolve(%q) = %q，想要 %q", c.in, got, c.want)
			}
		})
	}
}

// 【必记】Resolve 必须"笨"：它不做模糊匹配、不剥括号、不脑补。
//
// 因为它一旦聪明到能圆上模型所有的乱写，你就永远发现不了 prompt 有问题。
// 这个测试把"笨"这个设计固定住：改代码时谁想加个模糊匹配，这条就会红。
func TestResolve_MustStayStupid(t *testing.T) {
	book := mustTagBook(t,
		"并发/channel 关闭与阻塞",
	)

	// 这几种都该老实落到「未归类」，而不是被"聪明地"匹配上
	stupidCases := []string{
		"并发/channel关闭与阻塞",       // 少了空格
		"并发/channel 关闭与阻塞（待确认）", // 带了括号备注
		"channel 关闭与阻塞",         // 少了父级前缀
		"并发/Channel 关闭与阻塞",      // 大小写不同
	}
	for _, in := range stupidCases {
		t.Run(in, func(t *testing.T) {
			got := book.Resolve(in)
			if got == "并发/channel 关闭与阻塞" {
				t.Fatalf("Resolve(%q) 不该被模糊匹配上——那就发现不了 prompt 有问题了", in)
			}
		})
	}
}

// LoadTags 读人手写的文件：注释、空行、重复都得自己挡掉。
func TestLoadTags_CleanUp(t *testing.T) {
	book := mustTagBook(t,
		"# 这是注释",
		"",
		"并发/channel 关闭与阻塞",
		"并发/channel 关闭与阻塞", // 重复
		"   ",              // 纯空白
		"表达/没讲取舍",
	)

	all := book.All()
	if len(all) != 2 {
		t.Fatalf("注释/空行/重复都该被挡掉，应该剩 2 条，实际 %d 条: %v", len(all), all)
	}
	if all[0] != "并发/channel 关闭与阻塞" || all[1] != "表达/没讲取舍" {
		t.Fatalf("顺序应该跟文件里一致，实际: %v", all)
	}
}

// 空文件要报错，不能静默跑成"全都被归到未归类"。
func TestLoadTags_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tags.txt")
	if err := os.WriteFile(path, []byte("# 只有注释\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTags(path); err == nil {
		t.Fatal("空标签库应该报错，不能静默继续")
	}
}
