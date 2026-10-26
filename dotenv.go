package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv 读取项目根 .env（KEY=VALUE 格式），写入进程环境变量。
//
// 对应 Python 版 python-dotenv 的 load_dotenv()。这里手写而不引第三方库，
// 顺便看清它做的事只有两件：解析文件 + os.Setenv。
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return // 没有 .env 就静默跳过（复制 .env.example 即可）
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue // 空行和注释
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue // 没有 = 的脏行
		}
		os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	// Scan 循环结束后必须查 Err：Scan 返回 false 除了正常 EOF，还可能是
	// 读文件出错。不查的话 .env 读到一半坏了会静默丢掉后面的配置行，
	// 排查时毫无线索（"key 明明写了为什么没生效"）。
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "读取 .env 失败:", err)
	}
}
