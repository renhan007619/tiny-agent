package main

import (
	"os"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	// ① 准备：开一个测试专用的库（不碰你真实的记忆文件）
	s, err := NewStore("test_memory.db")
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer os.Remove("test_memory.db") // 测完顺手删掉测试库文件
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
		t.Fatalf("搜\"篮球\"应命中 1 条，实际 %d 条: %#v", len(hits), hits)
	}
	if hits[0] != "Q: 你最喜欢的颜色是什么 A: 绿色" {
		t.Fatalf("命中内容不对: %#v", hits[0])
	}
}
