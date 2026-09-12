package main

type Budget struct {
	Window        int //模型上下文窗口
	MaxTokens     int //单次回复请求token上限
	MaxToolRounds int //
}
