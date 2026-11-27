package modeladapter

const (
	// ClaudeCodeVersion is the Claude CLI version model requests are sent as.
	// It is named in two places of a request, and both are built from this one
	// constant: the user agent, and the cc_version of the billing header block
	// (see anthropic_billing_header.go). Anthropic refuses a model that needs
	// a newer Claude Code than cc_version names, so this has to follow the CLI.
	ClaudeCodeVersion = "2.1.329"
	// ClaudeCodeUserAgent 用于将渠道模型请求伪装为 Claude Code 客户端。
	ClaudeCodeUserAgent = "claude-cli/" + ClaudeCodeVersion + " (external, cli)"
	// AnthropicClaudeCodeUserAgent 用于 Anthropic provider 的 Claude Code UA 兼容。
	// 与上游 defaultCustomHeaders 的 User-Agent 保持一致（real claude-cli 版本号）。
	AnthropicClaudeCodeUserAgent = ClaudeCodeUserAgent
)
