//go:build liveprobe

package cursoraccount

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

// 用真实 api2.cursor.sh 验证「导入 crsr_ / grok_ key → 可用身份 → 自动续期」整条链路。
// 运行：$env:CURSOR_LIVE_API_KEY='grok_...'; go test -tags liveprobe -run TestLiveImport -v ./internal/cursoraccount/
func TestLiveImport(t *testing.T) {
	apiKey := os.Getenv("CURSOR_LIVE_API_KEY")
	if apiKey == "" {
		t.Skip("CURSOR_LIVE_API_KEY not set")
	}
	manager := NewManager(t.TempDir(), "", &http.Client{Timeout: 20 * time.Second})
	ctx := context.Background()

	summary, err := manager.ImportToken(ctx, apiKey)
	if err != nil {
		t.Fatalf("ImportToken: %v", err)
	}
	fmt.Printf("imported account: email=%q authIdHint=%q current=%v\n",
		summary.Email, summary.AuthIDHint, summary.IsCurrent)

	if manager.Status().State != StateSignedIn {
		t.Fatalf("state = %q, want signed_in", manager.Status().State)
	}

	creds, _, err := manager.store.LoadCurrentCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.RefreshToken != apiKey {
		t.Fatal("persisted refresh token must be the raw api key so renewal can re-exchange")
	}
	fmt.Printf("persisted: accessToken=%s... refreshTokenIsRawKey=%v\n",
		creds.AccessToken[:20], creds.RefreshToken == apiKey)

	// 走真实 Authorization 路径，确认拿到的是可用 Bearer。
	authorization, err := manager.Authorization(ctx)
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	profile, err := manager.fetchProfile(ctx, authorization)
	if err != nil {
		t.Fatalf("fetchProfile with Authorization(): %v", err)
	}
	fmt.Printf("Authorization() -> GetMe email=%q\n", profile.GetEmail())

	// 重新构造 Manager，确认 key 落盘后仍能续期（不依赖内存态）。
	reloaded := NewManager(manager.store.root, "", &http.Client{Timeout: 20 * time.Second})
	reloadedAuthorization, err := reloaded.Authorization(ctx)
	if err != nil {
		t.Fatalf("reloaded Authorization: %v", err)
	}
	if _, err := reloaded.fetchProfile(ctx, reloadedAuthorization); err != nil {
		t.Fatalf("reloaded fetchProfile: %v", err)
	}
	fmt.Println("reloaded manager can still authenticate")
}
