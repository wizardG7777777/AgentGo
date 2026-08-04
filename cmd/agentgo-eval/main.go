// agentgo-eval 是 V6 起的独立开发工具：行为评测（preflight/run/record）已从普通
// AgentGo Release 二进制剥离（docs/nextUpgrade-V6.md §7.6），不作为 Release 产物发布。
// 构建：go build -o agentgo-eval ./cmd/agentgo-eval
package main

import (
	"os"

	"agentgo/internal/eval"
)

func main() {
	os.Exit(eval.CLI(os.Args[1:], os.Stdout, os.Stderr))
}
