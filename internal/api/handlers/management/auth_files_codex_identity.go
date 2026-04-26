package management

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	codexAccountIdentityPrefix = "account_id:"
	codexEmailIdentityPrefix   = "email:"
)

// saveCodexTokenRecord 在 management 回调落盘前，先按账号身份尝试复用已有 auth 文件。
// 这里故意不把“同账号”语义下沉到通用 filestore，而是只收口在 Codex management OAuth
// 这条链路，避免把 provider 特定规则扩散到通用存储层。
func (h *Handler) saveCodexTokenRecord(
	ctx context.Context,
	storage *codex.CodexTokenStorage,
	planType string,
	hashAccountID string,
) (string, error) {
	if h == nil {
		return "", fmt.Errorf("handler is nil")
	}
	if storage == nil {
		return "", fmt.Errorf("codex token storage is nil")
	}

	record := newCodexTokenRecord(storage, planType, hashAccountID)
	identity := codexAuthIdentity(storage.AccountID, storage.Email)

	h.codexPersistMu.Lock()
	defer h.codexPersistMu.Unlock()

	existing, err := h.findExistingCodexAuthByIdentity(ctx, identity)
	if err != nil {
		return "", err
	}
	if existing != nil {
		reuseExistingCodexAuthTarget(record, existing)
	}
	return h.saveTokenRecord(ctx, record)
}

// writeUploadedCodexAuthFile 在 management 上传 token JSON 时，按账号身份复用已有旧文件。
// 这样同账号但不同上传文件名不会再被误当成两条独立 auth。
func (h *Handler) writeUploadedCodexAuthFile(
	ctx context.Context,
	proposedPath string,
	auth *coreauth.Auth,
	data []byte,
) (string, error) {
	if h == nil {
		return "", fmt.Errorf("handler is nil")
	}
	if auth == nil {
		return "", fmt.Errorf("auth is nil")
	}

	identity := codexAuthIdentityFromMetadata(auth.Metadata)
	h.codexPersistMu.Lock()
	defer h.codexPersistMu.Unlock()

	existing, err := h.findExistingCodexAuthByIdentity(ctx, identity)
	if err != nil {
		return "", err
	}

	targetPath := filepath.Clean(proposedPath)
	if existing != nil {
		if reusedPath := h.codexAuthTargetPath(existing); reusedPath != "" {
			targetPath = reusedPath
		}
		reuseExistingUploadedCodexAuthTarget(auth, existing)
		if cleanedTarget := filepath.Clean(targetPath); cleanedTarget != filepath.Clean(proposedPath) {
			auth, err = h.buildAuthFromFileData(cleanedTarget, data)
			if err != nil {
				return "", err
			}
			reuseExistingUploadedCodexAuthTarget(auth, existing)
			targetPath = cleanedTarget
		}
	}

	if err = h.persistBuiltAuthFile(ctx, targetPath, auth, data); err != nil {
		return "", err
	}
	return filepath.Base(targetPath), nil
}

// newCodexTokenRecord 构造 management OAuth 成功后的默认持久化记录。
// 默认仍沿用当前文件名规则；只有命中同账号旧 auth 时，才会在保存前复用旧路径。
func newCodexTokenRecord(
	storage *codex.CodexTokenStorage,
	planType string,
	hashAccountID string,
) *coreauth.Auth {
	fileName := codex.CredentialFileName(storage.Email, planType, hashAccountID, true)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Storage:  storage,
		Metadata: map[string]any{
			"email":      strings.TrimSpace(storage.Email),
			"account_id": strings.TrimSpace(storage.AccountID),
		},
	}
}

// findExistingCodexAuthByIdentity 优先从当前 live manager 查同账号文件，
// 若 manager 尚未感知到最新文件，再回退扫描 authDir 磁盘快照兜底。
func (h *Handler) findExistingCodexAuthByIdentity(
	ctx context.Context,
	identity string,
) (*coreauth.Auth, error) {
	if h == nil || identity == "" {
		return nil, nil
	}

	var best *coreauth.Auth
	manager := h.currentAuthManager()
	if manager != nil {
		for _, auth := range manager.List() {
			if !codexAuthMatchesIdentity(auth, identity) {
				continue
			}
			best = preferOlderCodexAuth(best, auth)
		}
	}
	if best != nil {
		return best, nil
	}

	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return nil, nil
	}
	auths, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list auth store for codex duplicate detection failed: %w", err)
	}
	for _, auth := range auths {
		if !codexAuthMatchesIdentity(auth, identity) {
			continue
		}
		best = preferOlderCodexAuth(best, auth)
	}
	return best, nil
}

// reuseExistingCodexAuthTarget 把新 token 的保存目标切到旧 auth 文件，
// 从而把 management 的“再次登录”收口成同账号原地更新，而不是创建第二份文件。
func reuseExistingCodexAuthTarget(record *coreauth.Auth, existing *coreauth.Auth) {
	if record == nil || existing == nil {
		return
	}
	if id := strings.TrimSpace(existing.ID); id != "" {
		record.ID = id
	}
	if fileName := strings.TrimSpace(existing.FileName); fileName != "" {
		record.FileName = fileName
	}
	if path := codexAuthFilePath(existing); path != "" {
		if record.Attributes == nil {
			record.Attributes = make(map[string]string, 2)
		}
		record.Attributes["path"] = path
		record.Attributes["source"] = path
	}
	record.Disabled = existing.Disabled
	if record.Disabled {
		record.Status = coreauth.StatusDisabled
	}
	record.ProxyURL = strings.TrimSpace(existing.ProxyURL)
	if record.Metadata == nil {
		record.Metadata = make(map[string]any, 3)
	}
	if existing.Metadata != nil {
		if registeredAt, ok := existing.Metadata[coreauth.FirstRegisteredAtMetadataKey]; ok {
			record.Metadata[coreauth.FirstRegisteredAtMetadataKey] = registeredAt
		}
	}
	if record.Disabled {
		record.Metadata["disabled"] = true
	}
}

// reuseExistingUploadedCodexAuthTarget 会把上传文件映射到旧 auth 身份，
// 但只在上传内容本身未显式声明时，保留旧的本地管理字段。
func reuseExistingUploadedCodexAuthTarget(auth *coreauth.Auth, existing *coreauth.Auth) {
	if auth == nil || existing == nil {
		return
	}
	if id := strings.TrimSpace(existing.ID); id != "" {
		auth.ID = id
	}
	if fileName := strings.TrimSpace(existing.FileName); fileName != "" {
		auth.FileName = fileName
	}
	if path := codexAuthFilePath(existing); path != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 2)
		}
		auth.Attributes["path"] = path
		auth.Attributes["source"] = path
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any, 3)
	}
	if existing.Metadata != nil {
		if registeredAt, ok := existing.Metadata[coreauth.FirstRegisteredAtMetadataKey]; ok {
			auth.Metadata[coreauth.FirstRegisteredAtMetadataKey] = registeredAt
		}
		if _, ok := auth.Metadata["disabled"]; !ok && existing.Disabled {
			auth.Metadata["disabled"] = true
			auth.Disabled = true
			auth.Status = coreauth.StatusDisabled
		}
		if _, ok := auth.Metadata["proxy_url"]; !ok {
			if proxyURL := strings.TrimSpace(existing.ProxyURL); proxyURL != "" {
				auth.Metadata["proxy_url"] = proxyURL
				auth.ProxyURL = proxyURL
			}
		}
	}
	preserveExistingCodexPlanType(auth, existing)
}

func preserveExistingCodexPlanType(auth *coreauth.Auth, existing *coreauth.Auth) {
	if auth == nil || existing == nil {
		return
	}
	if coreauth.AuthChatGPTPlanType(auth) != "" {
		return
	}
	planType := coreauth.AuthChatGPTPlanType(existing)
	if planType == "" {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string, 3)
	}
	auth.Attributes["plan_type"] = planType
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any, 1)
	}
	if _, ok := auth.Metadata["plan_type"]; !ok {
		auth.Metadata["plan_type"] = planType
	}
}

// preferOlderCodexAuth 在多份历史重复文件并存时，优先复用“最早入池”的那份，
// 保证 auth 身份与 fill-first 观察顺序尽量稳定；若时间相同，再退回 ID 字典序。
func preferOlderCodexAuth(best *coreauth.Auth, candidate *coreauth.Auth) *coreauth.Auth {
	if best == nil {
		return candidate
	}
	if candidate == nil {
		return best
	}
	bestAt, bestOK := coreauth.FirstRegisteredAt(best)
	candidateAt, candidateOK := coreauth.FirstRegisteredAt(candidate)
	switch {
	case candidateOK && bestOK && !candidateAt.Equal(bestAt):
		if candidateAt.Before(bestAt) {
			return candidate
		}
		return best
	case candidateOK && !bestOK:
		return candidate
	case !candidateOK && bestOK:
		return best
	case strings.TrimSpace(candidate.ID) < strings.TrimSpace(best.ID):
		return candidate
	default:
		return best
	}
}

// codexAuthMatchesIdentity 只匹配真正的 Codex 文件型 auth，避免把 config/runtime-only
// 的其它 codex 实体误当成 management OAuth 落盘目标。
func codexAuthMatchesIdentity(auth *coreauth.Auth, identity string) bool {
	if auth == nil || identity == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if codexAuthFilePath(auth) == "" && strings.TrimSpace(auth.FileName) == "" {
		return false
	}
	return codexAuthIdentityFromMetadata(auth.Metadata) == identity
}

func codexAuthIdentityFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	accountID := stringValue(metadata, "account_id")
	if accountID == "" {
		if rawIDToken := stringValue(metadata, "id_token"); rawIDToken != "" {
			if claims, err := codex.ParseJWTToken(rawIDToken); err == nil && claims != nil {
				accountID = strings.TrimSpace(claims.GetAccountID())
			}
		}
	}
	return codexAuthIdentity(accountID, stringValue(metadata, "email"))
}

func codexAuthIdentity(accountID string, email string) string {
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		return codexAccountIdentityPrefix + accountID
	}
	if email = normalizeCodexIdentityEmail(email); email != "" {
		return codexEmailIdentityPrefix + email
	}
	return ""
}

func normalizeCodexIdentityEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func codexAuthFilePath(auth *coreauth.Auth) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	path := strings.TrimSpace(auth.Attributes["path"])
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func (h *Handler) codexAuthTargetPath(auth *coreauth.Auth) string {
	if path := codexAuthFilePath(auth); path != "" {
		return path
	}
	if auth == nil {
		return ""
	}
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		return ""
	}
	if filepath.IsAbs(fileName) {
		return filepath.Clean(fileName)
	}
	cfg := h.currentConfigSnapshot()
	if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
		return filepath.Clean(fileName)
	}
	return filepath.Clean(filepath.Join(cfg.AuthDir, fileName))
}
