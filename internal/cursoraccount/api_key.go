package cursoraccount

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Cursor 用户 API Key（Dashboard → API Keys，前缀 crsr_）不是 IDE 直接可用的
// Bearer 凭据：api2.cursor.sh 的 aiserver.v1.* 只接受 WorkOS JWT。
//
// 两者之间的桥是 /auth/exchange_user_api_key：Authorization 用原始 key，
// body 是空 JSON 对象，换一对
// type=api_key_token 的 JWT（accessToken 与 refreshToken 是同一枚 token），
// 有效期约 1 小时。该 JWT 可直接作为 IDE 控制面与模型面的 Bearer 身份。
//
// 关键约束：/oauth/token 无法续期这类 token（返回 shouldLogout=true），
// 所以续期只能「用原始 key 重新 exchange」。因此账号库里把原始 crsr_ key
// 存进 RefreshToken 字段——它是唯一可长期复用的续期凭据，而 AccessToken
// 保存派生出的短期 JWT。
const (
	cursorAPIKeyPrefix        = "crsr_"
	apiKeyExchangePath        = "/auth/exchange_user_api_key"
	maxAPIKeyExchangeRespSize = 64 * 1024
)

// IsCursorAPIKey 判断一个字符串是否是 Cursor 用户 API Key。
func IsCursorAPIKey(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), cursorAPIKeyPrefix)
}

type apiKeyExchangeResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// ExchangeCursorAPIKey 把 crsr_ key 换成 IDE 面可用的短期 JWT。
// 返回的 credentials 保留原始 key 作为续期凭据。
func (manager *Manager) ExchangeCursorAPIKey(ctx context.Context, apiKey string) (credentials, error) {
	apiKey = strings.TrimSpace(apiKey)
	if !IsCursorAPIKey(apiKey) {
		return credentials{}, accountError("account_import_invalid_api_key", "cursor api key must start with crsr_")
	}
	// The agent host sends Authorization: Bearer <api key> and an empty JSON
	// object. The key is not a body field. See loginWithApiKey in
	// cursor-agent-host/dist/6907.js.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backendURL+apiKeyExchangePath, strings.NewReader("{}"))
	if err != nil {
		return credentials{}, err
	}
	req.Header.Set("authorization", bearer(apiKey))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")

	resp, err := manager.client.Do(req)
	if err != nil {
		return credentials{}, wrapAccountError("account_import_api_key_unreachable", "cursor api key exchange failed", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIKeyExchangeRespSize))
	if err != nil {
		return credentials{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return credentials{}, accountError("account_import_api_key_rejected", "cursor api key was rejected")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return credentials{}, accountError("account_import_api_key_rejected", fmt.Sprintf("cursor api key exchange returned HTTP %d", resp.StatusCode))
	}
	exchanged := apiKeyExchangeResponse{}
	if err := json.Unmarshal(body, &exchanged); err != nil {
		return credentials{}, accountError("account_import_api_key_invalid_response", "cursor api key exchange response is invalid")
	}
	accessToken := strings.TrimSpace(exchanged.AccessToken)
	if accessToken == "" {
		return credentials{}, accountError("account_import_api_key_invalid_response", "cursor api key exchange response has no access token")
	}
	return credentials{AccessToken: accessToken, RefreshToken: apiKey}, nil
}

// renewCursorAPIKey 用保存的原始 key 重新换取 JWT，并在换到新身份时补全邮箱。
func (manager *Manager) renewCursorAPIKey(ctx context.Context, current credentials) (credentials, error) {
	renewed, err := manager.ExchangeCursorAPIKey(ctx, current.RefreshToken)
	if err != nil {
		return credentials{}, err
	}
	renewed.AuthID = current.AuthID
	renewed.Email = current.Email
	if renewed.Email == "" {
		if profile, profileErr := manager.fetchProfile(ctx, bearer(renewed.AccessToken)); profileErr == nil {
			renewed.Email = strings.TrimSpace(profile.GetEmail())
			renewed.AuthID = strings.TrimSpace(profile.GetAuthId())
		}
	}
	return renewed, nil
}
