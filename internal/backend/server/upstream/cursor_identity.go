package upstream

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Cursor sends its own version in every request header. Hardcoding it makes the
// proxy look like an ancient client, and the server rejects model traffic with
// ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT ("Update Required").
//
// Version/commit are read from the installed client's
// <install>/resources/app/product.json. Detection is cached briefly so
// per-request overhead stays negligible.
//
// The machine id that goes into x-cursor-checksum is NOT read from Cursor's
// state.vscdb; see machine_id.go. The proxy uses its own per-account / per-install
// random ids so accounts are not linkable to the host's serviceMachineId.

const (
	cursorClientVersionFallback = "3.30.14"
	cursorDetectionTTL          = 5 * time.Minute
)

type cursorClientIdentity struct {
	Version    string
	Commit     string
	detectedAt time.Time
}

var (
	cursorIdentityMu   sync.RWMutex
	cursorIdentityInfo cursorClientIdentity
)

// CursorClientVersion returns the detected Cursor version, or a fallback.
func CursorClientVersion() string {
	return cursorIdentity().Version
}

// CursorClientCommit returns the detected Cursor commit, or "".
func CursorClientCommit() string {
	return cursorIdentity().Commit
}

// CursorClientOS returns Electron's process.platform value for the host OS.
func CursorClientOS() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// CursorClientArch returns Electron's process.arch value for the host CPU.
func CursorClientArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	default:
		return "x64"
	}
}

// ApplyCursorClientHeaders attaches the same client identity headers as the
// installed Cursor IDE on control-plane and agent-plane requests.
func ApplyCursorClientHeaders(req *http.Request, authorization, contentType string) {
	if req == nil {
		return
	}
	identity := cursorIdentity()
	if strings.TrimSpace(authorization) != "" {
		req.Header.Set("authorization", authorization)
		req.Header.Set("x-cursor-checksum", BuildCursorChecksum(authorization))
	}
	req.Header.Set("x-cursor-client-type", "ide")
	req.Header.Set("x-cursor-client-version", identity.Version)
	if identity.Commit != "" {
		req.Header.Set("x-cursor-client-commit", identity.Commit)
	}
	req.Header.Set("x-cursor-client-os", CursorClientOS())
	req.Header.Set("x-cursor-client-arch", CursorClientArch())
	req.Header.Set("x-cursor-client-device-type", "desktop")
	req.Header.Set("x-ghost-mode", "false")
	req.Header.Set("connect-protocol-version", "1")
	if strings.TrimSpace(contentType) != "" {
		req.Header.Set("content-type", contentType)
	}
}

// RefreshCursorIdentity forces re-detection. Called when settings change.
func RefreshCursorIdentity() {
	cursorIdentityMu.Lock()
	cursorIdentityInfo = cursorClientIdentity{}
	cursorIdentityMu.Unlock()
}

func cursorIdentity() cursorClientIdentity {
	cursorIdentityMu.RLock()
	cached := cursorIdentityInfo
	cursorIdentityMu.RUnlock()
	if !cached.detectedAt.IsZero() && time.Since(cached.detectedAt) < cursorDetectionTTL {
		return cached
	}

	identity := cursorClientIdentity{
		Version:   cursorClientVersionFallback,
		detectedAt: time.Now(),
	}
	if product, ok := readCursorProductJSON(); ok {
		if version := strings.TrimSpace(product.Version); version != "" {
			identity.Version = version
		}
		identity.Commit = strings.TrimSpace(product.Commit)
	}

	cursorIdentityMu.Lock()
	cursorIdentityInfo = identity
	cursorIdentityMu.Unlock()
	return identity
}

type cursorProduct struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// cursorInstallRoots lists candidate Cursor install directories per platform.
func cursorInstallRoots() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "cursor"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Cursor"),
			filepath.Join(os.Getenv("PROGRAMFILES"), "Cursor"),
			filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Cursor"),
		}
	case "darwin":
		return []string{"/Applications/Cursor.app/Contents"}
	default:
		return []string{"/usr/share/cursor", "/opt/Cursor", "/opt/cursor"}
	}
}

// readCursorProductJSON reads version/commit from resources/app/product.json.
func readCursorProductJSON() (cursorProduct, bool) {
	for _, root := range cursorInstallRoots() {
		if strings.TrimSpace(root) == "" {
			continue
		}
		for _, rel := range []string{
			filepath.Join("resources", "app", "product.json"),
			filepath.Join("Resources", "app", "product.json"),
		} {
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				continue
			}
			product := cursorProduct{}
			if err := json.Unmarshal(data, &product); err != nil {
				continue
			}
			if strings.TrimSpace(product.Version) != "" {
				return product, true
			}
		}
	}
	return cursorProduct{}, false
}