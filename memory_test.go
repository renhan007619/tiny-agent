package main

import (
	"path/filepath"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	// ① 准备：每次测试用全新临时目录，从空库开始。
	// 为什么不能像旧版那样复用 test_memory.db + defer os.Remove：
	// 清理只在测试进程正常收尾时执行，一旦上次运行被中断（Ctrl+C/停测），
	// 旧库文件就带着旧行残留；NewStore 的 CREATE TABLE IF NOT EXISTS 不清旧数据，
	// Add 又不查重 -> 下次再插同样的内容就出现重复行、命中数超出断言。
	// t.TempDir() 每次生成唯一目录、由 go test 自动清理，从根上消除残留。
	s, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer s.Close()

	// ② 操作：写入两条不同话题的记忆
	if err := s.Add("Q: 你最喜欢的颜色是什么 A: 绿色"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := s.Add("Q: 推荐一部电影 A: 星际穿越"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// ③ 断言：搜"颜色"应该只命中第 1 条
	hits, err := s.Search("颜色", 3)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("搜\"颜色\"应命中 1 条，实际 %d 条: %#v", len(hits), hits)
	}
	if hits[0] != "Q: 你最喜欢的颜色是什么 A: 绿色" {
		t.Fatalf("命中内容不对: %#v", hits[0])
	}
}
