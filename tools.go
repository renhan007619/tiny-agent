package main

import "math/rand"

// ============ 1. 工具函数：Agent 的能力集 ============
//
// 对应 agent.py 第 1 块。思想不变：工具就是普通函数，Agent 能调用的"能力"。
//
// 和 Python 的一个结构差异（面试可讲）：
//   Python 靠 inspect 反射"函数签名 + 参数名 + 注解"自动生成工具说明书，
//   而 Go 的 reflect 拿不到函数参数名。因此约定：无参工具直接写函数
//   （如本文件的 getColor / getNumber），带参工具写成
//   func(T) (R, error) 且 T 为 struct（字段名/json tag 即参数名），
//   见 schema.go 的 functionSchema。这是 Go agent 生态的通用做法。

// getColor 示例工具：随机返回一个颜色。模拟 Agent 可以调用的外部能力。
func getColor() string {
	colors := []string{"red", "green", "blue", "yellow", "purple", "white", "black"}
	return colors[rand.Intn(len(colors))]
}

// getNumber 示例工具：随机返回 0-100 的整数。
func getNumber() int {
	return rand.Intn(101)
}
