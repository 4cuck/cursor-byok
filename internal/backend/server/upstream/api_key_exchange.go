package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	cursorAPIKeyPrefix     = "crsr_"
	grokAPIKeyPrefix       = "grok_"
	cursorAPIKeyExchangeEP = "https://api2.cursor.sh/auth/exchange_user_api_key"
	maxExchangeRespBytes   = 64 * 1024
)

var errExchangeRejected = errors.New("cursor api key exchange rejected")

// IsCursorAPIKey reports whether raw is a Cursor Dashboard user API key
// (legacy crsr_ prefix or fine-grained grok_ prefix).
func IsCursorAPIKey(raw string) bool {
	value := strings.TrimSpace(raw)
	for _, prefix := range [...]string{cursorAPIKeyPrefix, grokAPIKeyPrefix} {
		if strings.HasPrefix(value, prefix) {
			suffix := value[len(prefix):]
			return suffix != "" && !strings.ContainsAny(suffix, " \t\r\n|")
		}
	}
	return false
}

// exchangeCursorAPIKey 把 Cursor 用户 API Key（crsr_ / grok_ 前缀）换成 IDE 面可用的
// 短期 JWT。非 API Key 值直接返回空字符串，调用方据此保留原有行为。
//
// 背景：客户端只会用 refresh_token 调 /oauth/token。以 API Key 导入的账号，
// 其 refresh token 就是原始 key；把原始 key 原样当 access token 返回，客户端
// 后续的 aiserver.v1.* 请求会因不是 JWT 而失败。
func exchangeCursorAPIKey(reqCtx *RequestContext, refreshToken string) (string, error) {
	value := strings.TrimSpace(refreshToken)
	if !IsCursorAPIKey(value) {
		return "", nil
	}
	req, err := http.NewRequestWithContext(
		reqCtx.Request.Context(),
		http.MethodPost,
		cursorAPIKeyExchangeEP,
		strings.NewReader("{}"),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("authorization", "Bearer "+value)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")

	client := reqCtx.Deps.HTTPClient
	if client == nil {
		return "", errors.New("upstream http client is not configured")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeRespBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errExchangeRejected
	}
	exchanged := struct {
		AccessToken string `json:"accessToken"`
	}{}
	if err := json.Unmarshal(body, &exchanged); err != nil {
		return "", err
	}
	return strings.TrimSpace(exchanged.AccessToken), nil
}
