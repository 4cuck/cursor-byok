package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cursor/internal/appdata"

	"github.com/google/uuid"
)

// The x-cursor-checksum header ends with a machine id that identifies the
// device to Cursor. The proxy must never reuse the host's real serviceMachineId:
// doing so links every account routed through cursor-byok back to one physical
// machine. Instead each stored account carries its own stable, randomly
// generated id (resolved below) and anything without an account falls back to a
// single per-install random id. Both are generated once and then kept, so an
// account only ever presents one device and never trips "too many computers".

var (
	machineIDResolverMu sync.RWMutex
	machineIDResolver   func(authorization string) string
)

// SetMachineIDResolver installs the per-account machine id resolver. The Cursor
// account manager registers its lookup here so BuildCursorChecksum can map an
// Authorization bearer token to that account's stable machine id. A resolver
// return of "" means "no stored account for this token"; callers then use the
// per-install id.
func SetMachineIDResolver(resolver func(authorization string) string) {
	machineIDResolverMu.Lock()
	machineIDResolver = resolver
	machineIDResolverMu.Unlock()
}

// ChecksumMachineID returns the machine id to embed in x-cursor-checksum for a
// request carrying the given Authorization header. Per-account ids take
// precedence; unmatched requests fall back to the stable per-install id. Cursor's
// own serviceMachineId is intentionally never consulted here.
func ChecksumMachineID(authorization string) string {
	machineIDResolverMu.RLock()
	resolver := machineIDResolver
	machineIDResolverMu.RUnlock()
	if resolver != nil {
		if id := strings.TrimSpace(resolver(authorization)); id != "" {
			return id
		}
	}
	return InstallMachineID()
}

// installMachineID is a random uuid persisted once under the app data dir. It
// replaces Cursor's serviceMachineId for requests that are not tied to a stored
// account, and stays constant across restarts so those requests always look
// like the same single device.
var (
	installMachineIDOnce sync.Once
	installMachineIDVal  string
)

// InstallMachineID returns the stable per-install machine id, creating and
// persisting it on first use.
func InstallMachineID() string {
	installMachineIDOnce.Do(func() {
		installMachineIDVal = loadOrCreateInstallMachineID(appdata.MachineIDFilePath())
	})
	return installMachineIDVal
}

func loadOrCreateInstallMachineID(path string) string {
	path = strings.TrimSpace(path)
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}
	id := uuid.NewString()
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			// A write failure only costs persistence: the process keeps this id in
			// memory, so the checksum stays stable until the next restart.
			_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
		}
	}
	return id
}
