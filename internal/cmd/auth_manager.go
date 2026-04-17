package cmd

import (
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
)

// newAuthManager 创建基于文件存储的认证管理器，
// 并注册当前仍支持的交互式登录提供商。
func newAuthManager() *sdkAuth.Manager {
	store := sdkAuth.GetTokenStore()
	manager := sdkAuth.NewManager(store,
		sdkAuth.NewGeminiAuthenticator(),
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewIFlowAuthenticator(),
		sdkAuth.NewAntigravityAuthenticator(),
		sdkAuth.NewKimiAuthenticator(),
	)
	return manager
}
