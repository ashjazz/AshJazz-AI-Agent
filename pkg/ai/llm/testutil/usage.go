package testutil

import "github.com/ashjazz/Longtermism/pkg/ai/llm"

// MustReportedUsage 仅用于测试 fixture：无效计数是测试数据错误，应立即暴露。
// 生产 adapter 必须使用返回 error 的构造器，不能在处理外部输入时使用此 helper。
func MustReportedUsage(summary llm.Usage) llm.ProviderUsage {
	usage, err := llm.NewReportedProviderUsage(summary)
	if err != nil {
		panic("invalid reported usage test fixture")
	}
	return usage
}
