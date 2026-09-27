package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============ read_file 测试 ============
//
// 测的重点不是"能读文件"（那是一行 os.ReadFile 的事），而是**拒绝路径**：
// 越界、目录、超限。这些分支决定模型拿到的是 is_error 的 tool_result，
// 还是一份它不该看到、或不该整份读进来的数据。
//
// readFileUnder 存在的唯一理由就是这里——测试可以自己造一个临时 root，
// 既不必往仓库塞 1MB 文件，也不会真去读用户机器上的东西。

// openTempRoot 造一个临时目录当 root，t.Cleanup 负责关闭。
func openTempRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("打开临时 root 失败: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func TestReadFileUnder(t *testing.T) {
	root, dir := openTempRoot(t)
	const content = "第一行\n第二行 hello\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o600); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("准备测试目录失败: %v", err)
	}

	t.Run("正常读取·内容一致", func(t *testing.T) {
		got, err := readFileUnder(root, "a.txt")
		if err != nil {
			t.Fatalf("应当成功，实际 %v", err)
		}
		if got != content {
			t.Fatalf("内容不一致: %q", got)
		}
	})

	t.Run("前后空格被容忍", func(t *testing.T) {
		// 模型经常在路径两边带空格，这属于格式噪音，不该变成一次失败调用。
		got, err := readFileUnder(root, "  a.txt  ")
		if err != nil || got != content {
			t.Fatalf("带空格的路径应当正常读取：got=%q, err=%v", got, err)
		}
	})

	t.Run("空路径被拒", func(t *testing.T) {
		_, err := readFileUnder(root, "   ")
		if err == nil || !strings.Contains(err.Error(), "不能为空") {
			t.Fatalf("空路径必须报错，实际 %v", err)
		}
	})

	t.Run("绝对路径被拒", func(t *testing.T) {
		_, err := readFileUnder(root, filepath.Join(dir, "a.txt"))
		if err == nil || !strings.Contains(err.Error(), "绝对路径") {
			t.Fatalf("绝对路径必须报错，实际 %v", err)
		}
	})

	t.Run("目录被拒", func(t *testing.T) {
		// 只断言"失败"，不断言具体分支：os.Root 或 Stat 任一条拦下都算对，
		// 重要的是绝不能把一段目录流当内容塞进上下文。
		if got, err := readFileUnder(root, "sub"); err == nil {
			t.Fatalf("读目录必须失败，实际返回 %q", got)
		}
	})

	t.Run("越界被拒", func(t *testing.T) {
		// os.Root 的语义保证：.. 不许越出 root。
		if got, err := readFileUnder(root, "../a.txt"); err == nil {
			t.Fatalf("越出 root 的路径必须失败，实际返回 %q", got)
		}
	})

	t.Run("不存在被拒", func(t *testing.T) {
		if _, err := readFileUnder(root, "nope.txt"); err == nil {
			t.Fatal("不存在的文件必须报错")
		}
	})

	t.Run("超过文件层闸门被拒", func(t *testing.T) {
		// 1MB+1：卡在"刚好超过"，钉住判据是 > 而不是 >=。
		big := filepath.Join(dir, "big.bin")
		if err := os.WriteFile(big, make([]byte, maxReadFileBytes+1), 0o600); err != nil {
			t.Fatalf("准备大文件失败: %v", err)
		}
		_, err := readFileUnder(root, "big.bin")
		if err == nil || !strings.Contains(err.Error(), "上限") {
			t.Fatalf("超限文件必须报上限错误，实际 %v", err)
		}
	})

	t.Run("恰好等于闸门上限·允许读取", func(t *testing.T) {
		ok := filepath.Join(dir, "exact.bin")
		if err := os.WriteFile(ok, make([]byte, maxReadFileBytes), 0o600); err != nil {
			t.Fatalf("准备边界文件失败: %v", err)
		}
		got, err := readFileUnder(root, "exact.bin")
		if err != nil {
			t.Fatalf("恰好 1MB 应当允许读取，实际 %v", err)
		}
		if len(got) != maxReadFileBytes {
			t.Fatalf("读到的长度 %d，want %d", len(got), maxReadFileBytes)
		}
	})
}

// TestReadFileFeedsTruncation 把两步串起来验一遍：读一个真·大文件 → 过 L0 闸门。
//
// 为什么必须有这条：上面那个测试只保证"能读"，context_assembler_test.go 只保证
// "能截"，但 L0 在真实运行里到底会不会被触发，取决于"有没有工具真能吐出一个
// 超配额的结果"。加 read_file 之前这个前提根本不成立——闸门写了却永远跑不到。
// 这条测试把那个前提钉死，L0 从此不是"写了但验不到"的代码。
func TestReadFileFeedsTruncation(t *testing.T) {
	root, dir := openTempRoot(t)
	const line = "这是一行会被截断的内容 abcdefg\n" // 48 字节
	big := strings.Repeat(line, 4000)    // 约 187KB
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatalf("准备大文件失败: %v", err)
	}

	raw, err := readFileUnder(root, "big.txt")
	if err != nil {
		t.Fatalf("%d 字节的文件应当允许读取（文件层闸门是 1MB）: %v", len(big), err)
	}

	// 走真实链路：工具返回值 -> stringify -> 本轮的配额 -> 闸门
	quota := toolResultQuota(DefaultBudget().ToolResultReserve, 1)
	text, cut := truncateToolResult(stringify(raw), quota)
	if !cut {
		t.Fatalf("读 %d 字节（约 %d token）必须触发 L0 截断，quota=%d",
			len(raw), estimateTextTokens(raw), quota)
	}
	if got := estimateTextTokens(text); got > quota {
		t.Fatalf("截断后仍占 %d token > quota %d —— 账本与闸门不同源", got, quota)
	}
	if !strings.Contains(text, "已截断") {
		t.Fatal("截断必须留痕，否则模型和人都不知道这条结果不完整")
	}
	t.Logf("✓ L0 被真实触发：%d 字节 / 约 %d token → %d token（quota %d），带截断标记",
		len(raw), estimateTextTokens(raw), estimateTextTokens(text), quota)
}
