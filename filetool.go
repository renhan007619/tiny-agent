package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ============ 真实工具：read_file ============
//
// 为什么加它：getColor / getNumber 永远吐不出超长结果，L0 闸门
// （agent.go 里 truncateToolResult 那一行）在真实运行中一次都不会被触发——
// 写了不等于验过。加一个会吐大结果的工具，闸门才有被测、被观察的机会；
// 顺带让这个 agent 从"只会报随机颜色"变成真能干点活。

// maxReadFileBytes 文件层闸门：防"读 GB 级文件把进程内存打爆"。
//
// 它和上下文层闸门（Budget.ToolResultReserve = 512 token，截断 + 留痕）分工不同，
// 不要合并成一个数：
//   - 文件层：超限直接报错拒绝。省 IO、省内存，这时候内容还压根没进上下文，
//     没必要为了"截断后能用"先把 1GB 读进内存。
//   - 上下文层：超限截断但保留。内容已经在上下文里了，模型还需要其中一部分
//     推进任务，砍掉整条反而更糟。
const maxReadFileBytes = 1 << 20 // 1MB

// readFileArgs read_file 的入参。
// 字段名即模型要填的参数名（schema.go 的 functionSchema 直接读 json tag 和
// description tag 生成 input_schema），所以 tag 是"给模型看的文档"，不是可选装饰。
type readFileArgs struct {
	Path string `json:"path" description:"要读取的文件路径，须位于当前工作目录内，例如 学习指南/agent面经.md"`
}

// readFile 作用：给模型"看到真实内容"的能力，同时是 L0 截断闸门的触发源。
// 入参：args.Path（相对进程工作目录的路径，不接受绝对路径）。
// 返回：文件全文；失败返回 error，由 execute 转成 is_error 的 tool_result
// （模型能看到自己错在哪，而不是调用悬空）。
func readFile(args readFileArgs) (string, error) {
	// Go 1.24+ 的 os.OpenRoot：以工作目录为根，路径穿越（..）与符号链接逃逸
	// 在语言层被封死。自己拼字符串判前缀是典型能被绕过的写法，不用。
	root, err := os.OpenRoot(".")
	if err != nil {
		return "", fmt.Errorf("打开工作目录失败: %w", err)
	}
	defer root.Close()
	return readFileUnder(root, args.Path)
}

// readFileUnder 在给定 root 下读取文件。
// 抽成独立函数只有一个理由：可测——测试传 t.TempDir() 当 root，
// 才能既覆盖正常路径又覆盖越界路径，而不必往仓库里塞一个 1MB 的测试文件。
func readFileUnder(root *os.Root, name string) (string, error) {
	rel := filepath.ToSlash(strings.TrimSpace(name))
	if rel == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("只接受工作目录内的相对路径，收到绝对路径 %q", name)
	}
	// 越界（..）与符号链接逃逸由 os.Root 自己拒绝，这里不重复实现一套。
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", name, err)
	}
	defer f.Close()

	if st, serr := f.Stat(); serr == nil {
		if st.IsDir() {
			return "", fmt.Errorf("%s 是目录，不是文件", name)
		}
		if st.Size() > maxReadFileBytes {
			return "", fmt.Errorf("%s 约 %d 字节，超过单次读取上限 %d 字节", name, st.Size(), maxReadFileBytes)
		}
	}
	// LimitReader 是上限兜底：Stat 与实际读取之间文件可能变大（TOCTOU），
	// 多读 1 字节用来判断"是不是真读完了"。
	b, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", name, err)
	}
	if len(b) > maxReadFileBytes {
		return "", fmt.Errorf("%s 超过单次读取上限 %d 字节", name, maxReadFileBytes)
	}
	return string(b), nil
}
