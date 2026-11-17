package upstream

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	// Registers the "sqlite" driver. Without this blank import sql.Open returns
	// a handle whose first query fails with "unknown driver", which is why the
	// machine id silently read as empty.
	_ "modernc.org/sqlite"
)

// Cursor sends its own version and machine id in every request header. Hardcoding
// them makes the proxy look like an ancient client, and the server rejects model
// traffic with ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT ("Update Required").
//
// Both values are read from the installed client:
//   - version/commit: <install>/resources/app/product.json
//   - machineId:      state.vscdb -> storage.serviceMachineId
//
// Detection is cached briefly so per-request overhead stays negligible.

const (
	cursorClientVersionFallback = "3.30.14"
	cursorDetectionTTL          = 5 * time.Minute
	cursorStateServiceMachineID = "storage.serviceMachineId"
)

type cursorClientIdentity struct {
	Version   string
	Commit    string
	MachineID string
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

// CursorMachineID returns the machine id Cursor uses in x-cursor-checksum.
func CursorMachineID() string {
	return cursorIdentity().MachineID
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
	if machineID := readCursorMachineID(); machineID != "" {
		identity.MachineID = machineID
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

// cursorStateDBPaths returns candidate state.vscdb locations.
func cursorStateDBPaths() []string {
	if override := strings.TrimSpace(os.Getenv("CURSOR_STATE_DB")); override != "" {
		return []string{override}
	}
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		return []string{
			filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb"),
			filepath.Join(appData, "cursor", "User", "globalStorage", "state.vscdb"),
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		return []string{filepath.Join(home, "Library", "Application Support", "Cursor",
			"User", "globalStorage", "state.vscdb")}
	default:
		home, _ := os.UserHomeDir()
		return []string{filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")}
	}
}

// readCursorMachineID reads storage.serviceMachineId from Cursor's state DB.
// Opened read-only: the live client must not be disturbed.
func readCursorMachineID() string {
	for _, path := range cursorStateDBPaths() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		machineID := readMachineIDFromStateDB(path)
		if machineID != "" {
			return machineID
		}
	}
	return ""
}

func readMachineIDFromStateDB(path string) string {
	// A Windows absolute path must become file:///C:/... — without the leading
	// slash SQLite parses "C:" as a URI scheme and the open silently fails.
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	db, err := sql.Open("sqlite", "file:"+uriPath+"?mode=ro")
	if err != nil {
		return ""
	}
	defer db.Close()
	// A read-only open still needs the driver to have loaded; surface nothing on
	// failure, callers fall back to the timestamp-only checksum.
	db.SetMaxOpenConns(1)

	var machineID string
	if err := db.QueryRow(
		`SELECT value FROM ItemTable WHERE key = ?`, cursorStateServiceMachineID,
	).Scan(&machineID); err != nil {
		return ""
	}
	return strings.TrimSpace(machineID)
}