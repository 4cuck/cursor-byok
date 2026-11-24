package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeHTTPClient 让测试用固定响应替代真实网络。
type fakeHTTPClient func(*http.Request) (*http.Response, error)

func (fn fakeHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type testResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *testResponseWriter) Header() http.Header    { return w.header }
func (w *testResponseWriter) WriteHeader(status int) { w.status = status }
func (w *testResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}

func newOAuthTestContext(client HTTPClient, body string) (*RequestContext, *testResponseWriter) {
	req, _ := http.NewRequest(http.MethodPost, "https://api2.cursor.sh/oauth/token", strings.NewReader(body))
	recorder := &testResponseWriter{header: make(http.Header)}
	return &RequestContext{
		ResponseWriter: recorder,
		Request:        req,
		RequestBody:    []byte(body),
		Deps:           &Dependencies{HTTPClient: client},
	}, recorder
}

func decodeOAuthResponse(t *testing.T, recorder *testResponseWriter) map[string]any {
	t.Helper()
	response := map[string]any{}
	if err := json.Unmarshal(recorder.body, &response); err != nil {
		t.Fatalf("decode response %q: %v", string(recorder.body), err)
	}
	return response
}

// API Key 账号的 refresh_token 是原始 crsr_ key，必须换成 JWT 再返回给客户端。
// 若原样回显，客户端会把原始 key 当 access token 保存，后续请求全部失效。
func TestMockOAuthExchangesCursorAPIKey(t *testing.T) {
	const exchangedJWT = "eyJhbGciOiJIUzI1NiJ9.exchanged.sig"
	var calledExchange bool
	client := fakeHTTPClient(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "exchange_user_api_key") {
			t.Fatalf("unexpected upstream call: %s", req.URL)
		}
		calledExchange = true
		if got := req.Header.Get("authorization"); got != "Bearer crsr_abcdef" {
			t.Fatalf("authorization = %q", got)
		}
		payload, err := json.Marshal(map[string]string{
			"accessToken":  exchangedJWT,
			"refreshToken": exchangedJWT,
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
			Request:    req,
		}, nil
	})

	reqCtx, recorder := newOAuthTestContext(client, `{"grant_type":"refresh_token","refresh_token":"crsr_abcdef"}`)
	if err := handleMockOAuth(reqCtx, &Route{}); err != nil {
		t.Fatalf("handleMockOAuth: %v", err)
	}
	if !calledExchange {
		t.Fatal("crsr_ refresh token must trigger an exchange")
	}
	response := decodeOAuthResponse(t, recorder)
	if response["access_token"] != exchangedJWT {
		t.Fatalf("access_token = %v, want exchanged JWT", response["access_token"])
	}
	if response["refresh_token"] != "crsr_abcdef" {
		t.Fatalf("refresh_token = %v, want the original api key", response["refresh_token"])
	}
	if response["shouldLogout"] != false {
		t.Fatalf("shouldLogout = %v, want false", response["shouldLogout"])
	}
}

// 非 crsr_ 的普通 refresh token 行为必须保持不变（原样回显）。
func TestMockOAuthPassesThroughNonAPIKeyRefreshToken(t *testing.T) {
	client := fakeHTTPClient(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("non-api-key refresh must not call exchange, got %s", req.URL)
		return nil, nil
	})
	reqCtx, recorder := newOAuthTestContext(client, `{"grant_type":"refresh_token","refresh_token":"opaque-refresh"}`)
	if err := handleMockOAuth(reqCtx, &Route{}); err != nil {
		t.Fatalf("handleMockOAuth: %v", err)
	}
	response := decodeOAuthResponse(t, recorder)
	if response["access_token"] != "opaque-refresh" {
		t.Fatalf("access_token = %v, want passthrough", response["access_token"])
	}
}

// 换取失败时必须回落原值，不能让客户端拿不到任何 token。
func TestMockOAuthFallsBackWhenExchangeFails(t *testing.T) {
	client := fakeHTTPClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"Invalid User API Key"}`)),
			Request:    req,
		}, nil
	})
	reqCtx, recorder := newOAuthTestContext(client, `{"grant_type":"refresh_token","refresh_token":"crsr_revoked"}`)
	if err := handleMockOAuth(reqCtx, &Route{}); err != nil {
		t.Fatalf("handleMockOAuth: %v", err)
	}
	response := decodeOAuthResponse(t, recorder)
	if response["access_token"] != "crsr_revoked" {
		t.Fatalf("access_token = %v, want fallback to original", response["access_token"])
	}
}

type stubSessionRefresher struct {
	access  string
	refresh string
	ok      bool
	got     string
}

func (stub *stubSessionRefresher) RefreshPresentedToken(_ context.Context, presented string) (string, string, bool) {
	stub.got = presented
	return stub.access, stub.refresh, stub.ok
}

func TestMockOAuthRenewsExpiredSessionToken(t *testing.T) {
	refresher := &stubSessionRefresher{access: "fresh-jwt", refresh: "crsr_kept", ok: true}
	reqCtx, recorder := newOAuthTestContext(fakeHTTPClient(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("expired session refresh must not exchange before the account store, got %s", req.URL)
		return nil, nil
	}), `{"grant_type":"refresh_token","refresh_token":"expired-jwt"}`)
	reqCtx.Deps.SessionRefresher = refresher
	if err := handleMockOAuth(reqCtx, &Route{}); err != nil {
		t.Fatalf("handleMockOAuth: %v", err)
	}
	if refresher.got != "expired-jwt" {
		t.Fatalf("presented token = %q", refresher.got)
	}
	response := decodeOAuthResponse(t, recorder)
	if response["access_token"] != "fresh-jwt" {
		t.Fatalf("access_token = %v", response["access_token"])
	}
	if response["refresh_token"] != "crsr_kept" {
		t.Fatalf("refresh_token = %v, want durable api key", response["refresh_token"])
	}
}

func TestMockOAuthExchangesGrokAPIKey(t *testing.T) {
	const exchangedJWT = "eyJhbGciOiJIUzI1NiJ9.grok-exchanged.sig"
	grokKey := "grok_" + strings.Repeat("a", 32) + "_" + strings.Repeat("b", 64)
	var calledExchange bool
	client := fakeHTTPClient(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "exchange_user_api_key") {
			t.Fatalf("unexpected upstream call: %s", req.URL)
		}
		calledExchange = true
		if got := req.Header.Get("authorization"); got != "Bearer "+grokKey {
			t.Fatalf("authorization = %q", got)
		}
		payload, err := json.Marshal(map[string]string{
			"accessToken":  exchangedJWT,
			"refreshToken": exchangedJWT,
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
			Request:    req,
		}, nil
	})

	reqCtx, recorder := newOAuthTestContext(client, `{"grant_type":"refresh_token","refresh_token":"`+grokKey+`"}`)
	if err := handleMockOAuth(reqCtx, &Route{}); err != nil {
		t.Fatalf("handleMockOAuth: %v", err)
	}
	if !calledExchange {
		t.Fatal("grok_ refresh token must trigger an exchange")
	}
	response := decodeOAuthResponse(t, recorder)
	if response["access_token"] != exchangedJWT {
		t.Fatalf("access_token = %v, want exchanged JWT", response["access_token"])
	}
	if response["refresh_token"] != grokKey {
		t.Fatalf("refresh_token = %v, want the original grok_ api key", response["refresh_token"])
	}
	if response["shouldLogout"] != false {
		t.Fatalf("shouldLogout = %v, want false", response["shouldLogout"])
	}
}

func TestIsCursorAPIKeyUpstream(t *testing.T) {
	cases := map[string]bool{
		"crsr_abcdef":                                                    true,
		"grok_abcdef":                                                    true,
		"grok_" + strings.Repeat("1", 32) + "_" + strings.Repeat("2", 64): true,
		"crsr_":                                                          false,
		"grok_":                                                          false,
		"grok_st|sub_123":                                                false,
		"xai-" + strings.Repeat("A", 80):                                 false,
		"opaque-refresh":                                                 false,
	}
	for input, want := range cases {
		if got := IsCursorAPIKey(input); got != want {
			t.Fatalf("IsCursorAPIKey(%q) = %v, want %v", input, got, want)
		}
	}
}
