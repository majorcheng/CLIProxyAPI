package codex

const (
	// RefreshTokenExpiredErrorCode 表示 refresh token 已过期，后续 refresh 无法再恢复。
	RefreshTokenExpiredErrorCode = "codex_refresh_token_expired"
	// RefreshTokenReusedErrorCode 表示 refresh token 已被轮转/复用，当前 token 链路已经耗尽。
	RefreshTokenReusedErrorCode = "codex_refresh_token_reused"
	// RefreshTokenRevokedErrorCode 表示 refresh token 被服务端作废或撤销。
	RefreshTokenRevokedErrorCode = "codex_refresh_token_revoked"
	// RefreshUnauthorizedErrorCode 表示 refresh 直接收到未知 401，按官方语义视为终态失败。
	RefreshUnauthorizedErrorCode = "codex_refresh_unauthorized"
	// UnauthorizedAfterRecoveryErrorCode 表示请求侧已经完成 refresh-retry，仍然返回 401。
	UnauthorizedAfterRecoveryErrorCode = "codex_unauthorized_after_recovery"
)
