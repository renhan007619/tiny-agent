package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============ 路线③ 外置 + 按 ID 召回 · 测试 ============
//
// 这个文件钉的是"承诺"，不是"实现"：断言全部围绕对外可见的行为
// （返回值、错误、文件是否被删），这样内部重构不会天天把测试弄红。
//
// 三条底线（如果只能保三条，就是这三条）：
//   1. 失效不返回空串（静默空内容是这类功能最危险的失败形态）
//   2. 召回一页不超过 pageTokens（超了会触发"召回结果被再次外置"的套娃）
//   3. 重启后句柄序号不重号（重号 = 静默读到别人的内容）

func TestResultStoreSaveReadRoundTrip(t *testing.T) {
	s, err := NewResultStore(t.TempDir(), "s1", 512)
	if err != nil {
		t.Fatal(err)
	}
	// 用短行（每行 "x\n" = 2 字节 ≈ 2 token），保证 100 行能装进一页；
	// 用长行会让 token 闸门提前截断，"第 1-100 行"这条断言就不成立了。
	h, err := s.Save("read_file", strings.Repeat("x\n", 2000))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(h, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 页眉必须能回答"我现在看到哪"，否则模型无法定位
	if !strings.Contains(got, "第 1-100 行") {
		t.Fatalf("页眉缺行号信息: %q", got)
	}
	// 页脚必须是可执行的下一页调用，而不是一句"可以继续读"
	if !strings.Contains(got, "offset=101") {
		t.Errorf("缺少翻页提示: %q", got)
	}
	// 底线 2：召回一页绝不能超过页闸门，否则会套娃外置
	if n := estimateTextTokens(got); n > 512 {
		t.Errorf("召回一页占 %d token，超过页闸门 512", n)
	}
}

func TestResultStoreInvalidHandle(t *testing.T) {
	s, _ := NewResultStore(t.TempDir(), "s1", 512)
	// 底线 1：无效句柄必须报错。返回空字符串的后果是模型以为"文件是空的"，
	// 然后基于"空"继续推理——静默空内容是外置类功能最危险的失败形态。
	if _, err := s.Read("res-99", 1, 10); err == nil {
		t.Fatal("无效句柄必须报错，不能返回空内容")
	}
}

func TestResultStoreReindexAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewResultStore(dir, "s1", 512)
	h1, _ := s1.Save("read_file", "abc\n")

	// 模拟进程重启：同一个目录、同一个会话，新建仓库
	s2, _ := NewResultStore(dir, "s1", 512)
	if _, err := s2.Read(h1, 1, 10); err != nil {
		t.Fatalf("重启后旧句柄应仍可用: %v", err)
	}
	// 底线 3：编号必须单调。重号会让模型拿着 res-1 读到别的内容
	//（静默读到错内容，比报错危险得多）。
	h2, _ := s2.Save("read_file", "def\n")
	if h2 == h1 {
		t.Fatal("重启后序号必须延续，不能覆盖旧句柄")
	}
}

func TestExternalizeKeepsHeadAndTail(t *testing.T) {
	s, _ := NewResultStore(t.TempDir(), "s1", 512)
	raw := "HEAD" + strings.Repeat("中间内容", 5000) + "TAIL"
	text, handle, cut := externalizeToolResult(s, "read_file", raw, 512)
	if !cut || handle == "" {
		t.Fatal("超长结果必须外置并给出句柄")
	}
	// 这是本方案最有辨识度的一条决策：砍中间、留两头
	//（头是格式/表头，尾是错误码/结论）。
	if !strings.Contains(text, "HEAD") || !strings.Contains(text, "TAIL") {
		t.Fatalf("外置后必须同时保留头部与尾部: %q", text)
	}
	if n := estimateTextTokens(text); n > 512 {
		t.Errorf("外置后占 %d token > 配额 512", n)
	}
	// 标记里的句柄必须真能用，否则模型拿到的是一个"看起来能读"的死号
	if _, err := s.Read(handle, 1, 10); err != nil {
		t.Errorf("标记里的句柄应能被召回: %v", err)
	}
}

func TestExternalizeUnderQuotaNoIO(t *testing.T) {
	s, _ := NewResultStore(t.TempDir(), "s1", 512)
	text, handle, cut := externalizeToolResult(s, "get_color", "red", 512)
	// 小结果零 IO、零改写：这条保证"外置不改变正常路径的行为"
	if cut || handle != "" || text != "red" {
		t.Fatal("未超配额时不该落盘、不该改内容")
	}
}

func TestResultStoreNilStoreFallsBackToTruncate(t *testing.T) {
	raw := strings.Repeat("中文内容", 2000)
	text, handle, cut := externalizeToolResult(nil, "read_file", raw, 512)
	// 钉住"可退化"：没有仓库时必须等价于旧的 L0 行为。
	// 这条同时保护"分步落地"——agent.go 接完而 main 还没注入仓库时，
	// 整个程序必须与改动前行为一致。
	if !cut || handle != "" {
		t.Fatal("没有仓库时必须退回 L0 纯截断")
	}
	if n := estimateTextTokens(text); n > 512 {
		t.Errorf("退回截断后仍超配额: %d", n)
	}
}

func TestCleanupRemovesOnlyOldDirs(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old-session")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	// 把目录 mtime 拨到 30 天前：Cleanup 判活看的就是目录 ModTime
	//（它成立的前提是 Save 每次都新建文件，见 Cleanup 注释里的隐性耦合）。
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	s, _ := NewResultStore(dir, "cur", 512)
	n, err := s.Cleanup(7 * 24 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("应清理 1 个过期目录，得到 %d, err=%v", n, err)
	}
	if _, serr := os.Stat(old); !os.IsNotExist(serr) {
		t.Fatal("过期目录未删除")
	}
	// 底线：当前会话目录永远不能删，否则等于把自己的工作记录删了
	if _, serr := os.Stat(s.sessionDir()); serr != nil {
		t.Fatal("当前会话目录不能被清理")
	}
}
