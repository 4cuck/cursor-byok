package cursoraccount

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

// fakeCursorAPIKeyTransport 复刻 api2.cursor.sh 上 API Key 相关端点的真实行为：
//   - /auth/exchange_user_api_key 用 Bearer + {apiKey} 换短期 JWT
//   - GetMe 用该 JWT 换身份
//   - /oauth/token 对这类 token 返回 shouldLogout=true（真实服务就是如此）
type fakeCursorAPIKeyTransport struct {
	apiKey      string
	exchangeHit int
	oauthHit    int
	tokenSeq    int
}

func (fake *fakeCursorAPIKeyTransport) roundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("unexpected http request in test")
	}
	switch {
	case strings.Contains(req.URL.Path, apiKeyExchangePath):
		fake.exchangeHit++
		fake.tokenSeq++
		if req.Header.Get("authorization") != bearer(fake.apiKey) {
			return staticHTTPResponse(http.StatusUnauthorized, "application/json",
				[]byte(`{"error":"Invalid User API Key"}`)), nil
		}
		body := map[string]string{}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body["apiKey"] != fake.apiKey {
			return staticHTTPResponse(http.StatusUnauthorized, "application/json",
				[]byte(`{"error":"Invalid User API Key"}`)), nil
		}
		token := testJWT(fake.tokenSeq, "api_key_token", 4102444800)
		payload, err := json.Marshal(map[string]string{
			"accessToken":  token,
			"refreshToken": token,
		})
		if err != nil {
			return nil, err
		}
		return staticHTTPResponse(http.StatusOK, "application/json", payload), nil
	case strings.Contains(req.URL.Path, "GetMe"):
		if !strings.HasPrefix(req.Header.Get("authorization"), "Bearer eyJ") {
			return staticHTTPResponse(http.StatusUnauthorized, "text/plain", []byte("not logged in")), nil
		}
		email := "cursor-user@example.com"
		authID := "user_test"
		body, err := proto.Marshal(&aiserverv1.GetMeResponse{
			AuthId: authID,
			Email:  &email,
		})
		if err != nil {
			return nil, err
		}
		return staticHTTPResponse(http.StatusOK, "application/proto", body), nil
	case strings.Contains(req.URL.Path, "/oauth/token"):
		fake.oauthHit++
		return staticHTTPResponse(http.StatusOK, "application/json",
			[]byte(`{"access_token":"","id_token":"","shouldLogout":true}`)), nil
	default:
		return nil, errors.New("unexpected http request in test: " + req.URL.String())
	}
}

func testJWT(seq int, tokenType string, exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"sub":  "google-oauth2|user_test",
		"type": tokenType,
		"exp":  exp,
		"seq":  seq,
	})
	if err != nil {
		panic(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func newAPIKeyTestManager(t *testing.T, fake *fakeCursorAPIKeyTransport) *Manager {
	t.Helper()
	return newTestManager(t, roundTripperFunc(fake.roundTrip))
}

func TestImportTokenExchangesCursorAPIKey(t *testing.T) {
	fake := &fakeCursorAPIKeyTransport{apiKey: "crsr_" + strings.Repeat("a", 64)}
	manager := newAPIKeyTestManager(t, fake)

	summary, err := manager.ImportToken(context.Background(), fake.apiKey)
	if err != nil {
		t.Fatalf("ImportToken: %v", err)
	}
	if fake.exchangeHit != 1 {
		t.Fatalf("expected one exchange call, got %d", fake.exchangeHit)
	}
	if summary.Email != "cursor-user@example.com" {
		t.Fatalf("email = %q", summary.Email)
	}

	// 原始 key 必须作为续期凭据保存，而不是把 1 小时 JWT 当 refresh token。
	creds, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.RefreshToken != fake.apiKey {
		t.Fatalf("stored refresh token should be the raw api key, got %q", creds.RefreshToken)
	}
	if !strings.HasPrefix(creds.AccessToken, "eyJ") {
		t.Fatalf("stored access token should be the exchanged JWT, got %q", creds.AccessToken)
	}
	if IsCursorAPIKey(creds.AccessToken) {
		t.Fatal("access token must not be the raw api key")
	}
}

func TestAuthorizationRenewsCursorAPIKeyWithoutOAuth(t *testing.T) {
	fake := &fakeCursorAPIKeyTransport{apiKey: "crsr_" + strings.Repeat("b", 64)}
	manager := newAPIKeyTestManager(t, fake)

	if _, err := manager.ImportToken(context.Background(), fake.apiKey); err != nil {
		t.Fatalf("ImportToken: %v", err)
	}

	// 换成已过期 JWT，强制走续期分支。
	creds, accountID, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	creds.AccessToken = testJWT(99, "api_key_token", 1)
	if err := manager.store.UpdateCredentials(accountID, creds); err != nil {
		t.Fatal(err)
	}
	manager.adoptCurrentFromStore()

	authorization, err := manager.Authorization(context.Background())
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	if !strings.HasPrefix(authorization, "Bearer eyJ") {
		t.Fatalf("authorization = %q", authorization)
	}
	if fake.oauthHit != 0 {
		t.Fatalf("api key accounts must never hit /oauth/token, got %d calls", fake.oauthHit)
	}
	if fake.exchangeHit != 2 {
		t.Fatalf("expected a second exchange for renewal, got %d total", fake.exchangeHit)
	}

	// 续期后原始 key 必须原样保留，否则第二次续期会失败。
	renewed, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if renewed.RefreshToken != fake.apiKey {
		t.Fatalf("renewal dropped the api key: %q", renewed.RefreshToken)
	}
}

func TestRefreshPresentedTokenRenewsExpiredAPIKeyJWT(t *testing.T) {
	fake := &fakeCursorAPIKeyTransport{apiKey: "crsr_" + strings.Repeat("d", 64)}
	manager := newAPIKeyTestManager(t, fake)
	if _, err := manager.ImportToken(context.Background(), fake.apiKey); err != nil {
		t.Fatalf("ImportToken: %v", err)
	}
	creds, accountID, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	expired := testJWT(7, "api_key_token", 1)
	creds.AccessToken = expired
	if err := manager.store.UpdateCredentials(accountID, creds); err != nil {
		t.Fatal(err)
	}

	access, refresh, ok := manager.RefreshPresentedToken(context.Background(), expired)
	if !ok {
		t.Fatal("expired api key jwt should renew")
	}
	if !strings.HasPrefix(access, "eyJ") {
		t.Fatalf("access = %q", access)
	}
	if refresh != fake.apiKey {
		t.Fatalf("refresh = %q, want raw api key", refresh)
	}
	if fake.oauthHit != 0 {
		t.Fatalf("presented jwt must not call /oauth/token, got %d", fake.oauthHit)
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != fake.apiKey {
		t.Fatalf("stored refresh = %q", stored.RefreshToken)
	}
	if stored.AccessToken != access {
		t.Fatalf("stored access = %q, want %q", stored.AccessToken, access)
	}
}

func TestUpdateCredentialsKeepsCursorAPIKey(t *testing.T) {
	fake := &fakeCursorAPIKeyTransport{apiKey: "crsr_" + strings.Repeat("e", 64)}
	manager := newAPIKeyTestManager(t, fake)
	if _, err := manager.ImportToken(context.Background(), fake.apiKey); err != nil {
		t.Fatalf("ImportToken: %v", err)
	}
	creds, accountID, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	creds.RefreshToken = testJWT(1, "api_key_token", 4102444800)
	if err := manager.store.UpdateCredentials(accountID, creds); err != nil {
		t.Fatal(err)
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != fake.apiKey {
		t.Fatalf("refresh token was replaced by a jwt: %q", stored.RefreshToken)
	}
}

func TestImportTokenRejectsRejectedAPIKey(t *testing.T) {
	fake := &fakeCursorAPIKeyTransport{apiKey: "crsr_" + strings.Repeat("c", 64)}
	manager := newAPIKeyTestManager(t, fake)

	_, err := manager.ImportToken(context.Background(), "crsr_wrong")
	if err == nil {
		t.Fatal("expected rejection")
	}
	var coded *codedError
	if !errors.As(err, &coded) || coded.code != "account_import_api_key_rejected" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsCursorAPIKey(t *testing.T) {
	cases := map[string]bool{
		"crsr_abc":                 true,
		"  crsr_abc  ":             true,
		"eyJhbGciOiJIUzI1NiJ9.x.y": false,
		"":                         false,
		"crs":                      false,
		"user_01ABC::eyJhbGciOiJ9": false,
	}
	for input, want := range cases {
		if got := IsCursorAPIKey(input); got != want {
			t.Fatalf("IsCursorAPIKey(%q) = %v, want %v", input, got, want)
		}
	}
}
