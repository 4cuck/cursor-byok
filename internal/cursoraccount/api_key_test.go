package cursoraccount

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

// fakeCursorAPIKeyTransport 复刻 api2.cursor.sh 上 API Key 相关端点的真实行为：
//   - /auth/exchange_user_api_key 用 Bearer 原始 key 和空 JSON 对象换短期 JWT
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
		rawBody, err := io.ReadAll(io.LimitReader(req.Body, 1024))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(rawBody)) != "{}" {
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

func TestForceRefreshReplacesUnexpiredAPIKeyJWT(t *testing.T) {
	fake := &fakeCursorAPIKeyTransport{apiKey: "crsr_" + strings.Repeat("f", 64)}
	manager := newAPIKeyTestManager(t, fake)
	if _, err := manager.ImportToken(context.Background(), fake.apiKey); err != nil {
		t.Fatalf("ImportToken: %v", err)
	}
	before := fake.exchangeHit
	authorization, err := manager.Authorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fake.exchangeHit != before {
		t.Fatal("unexpired jwt must not be exchanged by Authorization")
	}
	renewed, err := manager.ForceRefresh(context.Background(), manager.CurrentAccountID())
	if err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	if renewed == authorization {
		t.Fatal("force refresh should mint a new access token")
	}
	if fake.exchangeHit != before+1 {
		t.Fatalf("exchange calls = %d, want %d", fake.exchangeHit, before+1)
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != fake.apiKey {
		t.Fatalf("refresh token = %q, want the raw api key", stored.RefreshToken)
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

func TestGrokAPIKeyFullLifecycle(t *testing.T) {
	grokKey := "grok_" + strings.Repeat("a", 32) + "_" + strings.Repeat("b", 64)
	fake := &fakeCursorAPIKeyTransport{apiKey: grokKey}
	manager := newAPIKeyTestManager(t, fake)

	summary, err := manager.ImportToken(context.Background(), grokKey)
	if err != nil {
		t.Fatalf("ImportToken(grok_): %v", err)
	}
	if fake.exchangeHit != 1 {
		t.Fatalf("expected one exchange call, got %d", fake.exchangeHit)
	}
	if summary.Email != "cursor-user@example.com" {
		t.Fatalf("email = %q", summary.Email)
	}
	if summary.CredentialType != "api_key" {
		t.Fatalf("credentialType = %q, want api_key", summary.CredentialType)
	}
	if !manager.CurrentAccountUsesAPIKey() || !manager.UsesAPIKey(summary.ID) {
		t.Fatal("grok_ account must report UsesAPIKey == true")
	}

	creds, accountID, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.RefreshToken != grokKey {
		t.Fatalf("stored refresh token should be the raw grok_ api key, got %q", creds.RefreshToken)
	}
	if !strings.HasPrefix(creds.AccessToken, "eyJ") || IsCursorAPIKey(creds.AccessToken) {
		t.Fatalf("stored access token should be the exchanged JWT, got %q", creds.AccessToken)
	}

	// Expire the derived JWT and ensure Authorization re-exchanges the grok_ key without calling /oauth/token.
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
		t.Fatalf("grok_ api key accounts must never hit /oauth/token, got %d calls", fake.oauthHit)
	}
	if fake.exchangeHit != 2 {
		t.Fatalf("expected 2 exchange calls, got %d", fake.exchangeHit)
	}

	// RefreshPresentedToken should also renew an expired grok_-derived JWT and return the raw grok_ key as refresh.
	expired := testJWT(77, "api_key_token", 1)
	creds.AccessToken = expired
	if err := manager.store.UpdateCredentials(accountID, creds); err != nil {
		t.Fatal(err)
	}
	access, refresh, ok := manager.RefreshPresentedToken(context.Background(), expired)
	if !ok {
		t.Fatal("expired grok_ api key jwt should renew")
	}
	if !strings.HasPrefix(access, "eyJ") || refresh != grokKey {
		t.Fatalf("RefreshPresentedToken = (%q, %q), want jwt + raw grok_ key", access, refresh)
	}
	if fake.oauthHit != 0 {
		t.Fatalf("presented grok_ jwt must not call /oauth/token, got %d", fake.oauthHit)
	}

	// ForceRefresh must re-exchange an unexpired JWT while keeping the raw grok_ key.
	renewedAuth, err := manager.ForceRefresh(context.Background(), accountID)
	if err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	if renewedAuth == bearer(access) {
		t.Fatal("ForceRefresh should mint a new JWT")
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != grokKey {
		t.Fatalf("ForceRefresh dropped grok_ key: %q", stored.RefreshToken)
	}

	// UpdateCredentials must refuse overwriting durable grok_ key with a short-lived JWT.
	stored.RefreshToken = testJWT(1, "api_key_token", 4102444800)
	if err := manager.store.UpdateCredentials(accountID, stored); err != nil {
		t.Fatal(err)
	}
	afterUpdate, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if afterUpdate.RefreshToken != grokKey {
		t.Fatalf("refresh token was replaced by a jwt: %q", afterUpdate.RefreshToken)
	}
}

func TestRotateLegacyCrsrKeyToGrokKeyOnSameAccount(t *testing.T) {
	crsrKey := "crsr_" + strings.Repeat("1", 64)
	grokKey := "grok_" + strings.Repeat("2", 32) + "_" + strings.Repeat("3", 64)
	fake := &fakeCursorAPIKeyTransport{apiKey: crsrKey}
	manager := newAPIKeyTestManager(t, fake)

	first, err := manager.ImportToken(context.Background(), crsrKey)
	if err != nil {
		t.Fatalf("ImportToken(crsr_): %v", err)
	}
	fake.apiKey = grokKey
	second, err := manager.ImportToken(context.Background(), grokKey)
	if err != nil {
		t.Fatalf("ImportToken(grok_): %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same account id on key rotation: %q vs %q", first.ID, second.ID)
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != grokKey {
		t.Fatalf("refresh token = %q, want rotated grok_ key", stored.RefreshToken)
	}
}

func TestImportJSONExchangesGrokAPIKey(t *testing.T) {
	grokKey := "grok_" + strings.Repeat("4", 32) + "_" + strings.Repeat("5", 64)
	fake := &fakeCursorAPIKeyTransport{apiKey: grokKey}
	manager := newAPIKeyTestManager(t, fake)

	summaries, err := manager.ImportJSON(`{"accessToken":"` + grokKey + `","email":"grok@example.com","authId":"user_grok"}`)
	if err != nil {
		t.Fatalf("ImportJSON(grok_): %v", err)
	}
	if len(summaries) != 1 || summaries[0].CredentialType != "api_key" {
		t.Fatalf("summaries = %+v", summaries)
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != grokKey || !strings.HasPrefix(stored.AccessToken, "eyJ") {
		t.Fatalf("stored creds = %+v", stored)
	}
}

func TestImportTokenGrokFallsBackToJWTSubjectWhenGetMeFails(t *testing.T) {
	grokKey := "grok_" + strings.Repeat("6", 32) + "_" + strings.Repeat("7", 64)
	manager := newTestManager(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, apiKeyExchangePath):
			if req.Header.Get("authorization") != bearer(grokKey) {
				return staticHTTPResponse(http.StatusUnauthorized, "application/json", []byte(`{"error":"invalid"}`)), nil
			}
			token := testJWT(1, "api_key_token", 4102444800)
			payload, _ := json.Marshal(map[string]string{"accessToken": token, "refreshToken": token})
			return staticHTTPResponse(http.StatusOK, "application/json", payload), nil
		case strings.Contains(req.URL.Path, "GetMe"):
			return staticHTTPResponse(http.StatusForbidden, "text/plain", []byte("forbidden")), nil
		default:
			return nil, errors.New("unexpected http request: " + req.URL.String())
		}
	}))

	summary, err := manager.ImportToken(context.Background(), grokKey)
	if err != nil {
		t.Fatalf("ImportToken(grok_) with GetMe 403 should fall back to JWT subject: %v", err)
	}
	if summary.AuthIDHint != "google-oauth2|user_test" {
		t.Fatalf("authIdHint = %q, want google-oauth2|user_test", summary.AuthIDHint)
	}
	if summary.CredentialType != "api_key" {
		t.Fatalf("credentialType = %q, want api_key", summary.CredentialType)
	}
}

func TestImportTokenRejectsXAIAPIKeyWithoutNetworkCall(t *testing.T) {
	xaiKey := "xai-" + strings.Repeat("A", 80)
	var networkHits int
	manager := newTestManager(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		networkHits++
		return nil, errors.New("unexpected network call: " + req.URL.String())
	}))

	_, err := manager.ImportToken(context.Background(), xaiKey)
	if err == nil {
		t.Fatal("expected xai- key to be rejected")
	}
	var coded *codedError
	if !errors.As(err, &coded) || coded.code != "account_import_xai_api_key" {
		t.Fatalf("unexpected error: %v", err)
	}
	if networkHits != 0 {
		t.Fatalf("xai- key must be rejected locally without sending it upstream, got %d network calls", networkHits)
	}
}

func TestIsCursorAPIKey(t *testing.T) {
	cases := map[string]bool{
		"crsr_abc":                                                       true,
		"  crsr_abc  ":                                                   true,
		"grok_abc":                                                       true,
		"  grok_abc_123  ":                                               true,
		"grok_" + strings.Repeat("a", 32) + "_" + strings.Repeat("b", 64): true,
		"crsr_":                                                          false,
		"grok_":                                                          false,
		"grok_st|google-oauth2|123":                                      false,
		"grok_has space":                                                 false,
		"grok-4.7":                                                       false,
		"xai-" + strings.Repeat("A", 80):                                 false,
		"eyJhbGciOiJIUzI1NiJ9.x.y":                                       false,
		"":                                                               false,
		"crs":                                                            false,
		"grok":                                                           false,
		"user_01ABC::eyJhbGciOiJ9":                                       false,
	}
	for input, want := range cases {
		if got := IsCursorAPIKey(input); got != want {
			t.Fatalf("IsCursorAPIKey(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestImportTokenJWTReplacesStaleAPIKeyRefreshToken(t *testing.T) {
	crsrKey := "crsr_" + strings.Repeat("9", 64)
	fake := &fakeCursorAPIKeyTransport{apiKey: crsrKey}
	manager := newAPIKeyTestManager(t, fake)

	first, err := manager.ImportToken(context.Background(), crsrKey)
	if err != nil {
		t.Fatalf("ImportToken(crsr_): %v", err)
	}
	if !manager.UsesAPIKey(first.ID) {
		t.Fatal("expected initial account to use API key")
	}

	sessionJWT := testJWT(50, "session", 4102444800)
	second, err := manager.ImportToken(context.Background(), sessionJWT)
	if err != nil {
		t.Fatalf("ImportToken(session jwt): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected same account id %q, got %q", first.ID, second.ID)
	}
	if second.CredentialType != "oauth" {
		t.Fatalf("credentialType = %q, want oauth", second.CredentialType)
	}
	if manager.UsesAPIKey(second.ID) {
		t.Fatal("stale crsr_ refresh token must be cleared when importing a standalone JWT")
	}
	stored, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != sessionJWT || stored.RefreshToken != "" {
		t.Fatalf("stored creds = %+v, want session JWT with empty refresh token", stored)
	}
}

func TestRefreshPresentedTokenDoesNotOverwriteUnexpiredJWT(t *testing.T) {
	var oauthHits int
	manager := newTestManager(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "GetMe"):
			email := "jwt-user@example.com"
			body, _ := proto.Marshal(&aiserverv1.GetMeResponse{AuthId: "user_test", Email: &email})
			return staticHTTPResponse(http.StatusOK, "application/proto", body), nil
		case strings.Contains(req.URL.Path, "/oauth/token"):
			oauthHits++
			return staticHTTPResponse(http.StatusOK, "application/json",
				[]byte(`{"access_token":"","id_token":"","shouldLogout":true}`)), nil
		default:
			return nil, errors.New("unexpected http request: " + req.URL.String())
		}
	}))

	sessionJWT := testJWT(10, "session", 4102444800)
	if _, err := manager.ImportToken(context.Background(), sessionJWT); err != nil {
		t.Fatalf("ImportToken: %v", err)
	}

	// Cursor IDE calls /oauth/token on any JWT with <53 days left even when it
	// is still valid for weeks. Must NOT hit upstream or overwrite stored JWT.
	if access, refresh, ok := manager.RefreshPresentedToken(context.Background(), sessionJWT); ok || access != "" || refresh != "" {
		t.Fatalf("unexpired presented JWT should not trigger refresh, got (%q, %q, %v)", access, refresh, ok)
	}
	if oauthHits != 0 {
		t.Fatalf("unexpired presented JWT must not call /oauth/token, got %d hits", oauthHits)
	}

	// If IDE presents an older expired JWT for the same sub while our store
	// already has a fresher unexpired JWT, return the fresher stored JWT directly.
	olderJWT := testJWT(1, "session", 1)
	access, _, ok := manager.RefreshPresentedToken(context.Background(), olderJWT)
	if !ok || access != sessionJWT {
		t.Fatalf("expected fresher stored JWT directly, got (%q, ok=%v)", access, ok)
	}
	if oauthHits != 0 {
		t.Fatalf("returning fresher stored JWT must not call /oauth/token, got %d hits", oauthHits)
	}
}

func TestForceRefreshDoesNotWipeUnexpiredCurrentSessionOnShouldLogout(t *testing.T) {
	var oauthHits int
	manager := newTestManager(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/oauth/token") {
			oauthHits++
			return staticHTTPResponse(http.StatusOK, "application/json",
				[]byte(`{"access_token":"","id_token":"","shouldLogout":true}`)), nil
		}
		return nil, errors.New("unexpected http request: " + req.URL.String())
	}))

	unexpiredJWT := testJWT(20, "session", 4102444800)
	summary, err := manager.store.UpsertReplacingCredentials(credentials{
		AccessToken:  unexpiredJWT,
		RefreshToken: "revoked-oauth-refresh",
		AuthID:       "user_test",
		Email:        "oauth@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetCurrent(summary.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.ForceRefresh(context.Background(), summary.ID); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("ForceRefresh err = %v, want ErrNotSignedIn", err)
	}
	if oauthHits != 1 {
		t.Fatalf("oauthHits = %d, want 1", oauthHits)
	}
	if manager.CurrentAccountID() != summary.ID {
		t.Fatalf("CurrentAccountID was cleared on speculative ForceRefresh: got %q, want %q", manager.CurrentAccountID(), summary.ID)
	}
	auth, err := manager.Authorization(context.Background())
	if err != nil || auth != "Bearer "+unexpiredJWT {
		t.Fatalf("Authorization after failed ForceRefresh = (%q, %v), want unexpired JWT intact", auth, err)
	}
}

func TestRefreshOAuthSkipsNonRefreshableWebAndAPIKeyTokens(t *testing.T) {
	var oauthHits int
	manager := newTestManager(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		oauthHits++
		return nil, errors.New("unexpected http request: " + req.URL.String())
	}))

	for _, tokenType := range []string{"web", "api_key_token"} {
		webJWT := testJWT(30, tokenType, 4102444800)
		if _, _, err := manager.refreshOAuth(context.Background(), credentials{
			AccessToken:  webJWT,
			RefreshToken: webJWT,
		}); err == nil {
			t.Fatalf("expected refreshOAuth to reject type=%q token locally", tokenType)
		}
	}
	if oauthHits != 0 {
		t.Fatalf("non-refreshable tokens must never hit /oauth/token, got %d hits", oauthHits)
	}
}

func TestJWTTokenType(t *testing.T) {
	if got := JWTTokenType(testJWT(1, "web", 4102444800)); got != "web" {
		t.Fatalf("JWTTokenType(web) = %q", got)
	}
	if got := JWTTokenType("Bearer " + testJWT(1, "session", 4102444800)); got != "session" {
		t.Fatalf("JWTTokenType(Bearer session) = %q", got)
	}
	if got := JWTTokenType("not-a-jwt"); got != "" {
		t.Fatalf("JWTTokenType(non-jwt) = %q, want empty", got)
	}
}
