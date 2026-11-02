// tiny-agent 的命令行入口。
// 真正的代码都在 internal/core 里，这里只负责把它叫起来。
package main

import "tiny-agent/internal/core"

func main() {
	core.Main()
}
