package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cursor/internal/appdata"

	"github.com/google/uuid"
)

// The x-cursor-checksum header ends with a device identity that identifies the
// host to Cursor: `base64(timestamp) + machineId` plus `/macMachineId`. The proxy
// must never reuse the host's real serviceMachineId / macMachineId: doing so
// links every account routed through cursor-byok back to one physical machine.
// Instead each stored account carries its own stable, randomly generated pair
// (resolved below) and anything without an account falls back to a single
// per-install pair. All values are generated once and then kept, so an account
// only ever presents one device and never trips "too many computers".

// ChecksumIdentity is the machine id pair embedded in x-cursor-checksum.
type ChecksumIdentity struct {
	MachineID    string // serviceMachineId (uuid)
	MacMachineID string // macMachineId (64-hex)
}

var (
	checksumIdentityResolverMu sync.RWMutex
	checksumIdentityResolver   func(authorization string) ChecksumIdentity
)

// SetMachineIDResolver installs the per-account checksum identity resolver. The
// Cursor account manager registers its lookup here so BuildCursorChecksum can map
// an Authorization bearer token to that account's stable ids. A resolver returning
// an empty MachineID means "no stored account for this token"; callers then use
// the per-install identity.
func SetMachineIDResolver(resolver func(authorization string) ChecksumIdentity) {
	checksumIdentityResolverMu.Lock()
	checksumIdentityResolver = resolver
	checksumIdentityResolverMu.Unlock()
}

// checksumIdentityFor returns the machine id pair to embed in x-cursor-checksum
// for a request carrying the given Authorization header. Per-account ids take
// precedence; unmatched requests fall back to the stable per-install pair.
// Cursor's own serviceMachineId is intentionally never consulted here.
func checksumIdentityFor(authorization string) ChecksumIdentity {
	checksumIdentityResolverMu.RLock()
	resolver := checksumIdentityResolver
	checksumIdentityResolverMu.RUnlock()
	if resolver != nil {
		if identity := resolver(authorization); strings.TrimSpace(identity.MachineID) != "" {
			return identity
		}
	}
	return InstallChecksumIdentity()
}

// installIdentity is a random machine id pair persisted once under the app data
// dir. It replaces Cursor's serviceMachineId / macMachineId for requests that are
// not tied to a stored account, and stays constant across restarts so those
// requests always look like the same single device.
var (
	installIdentityOnce sync.Once
	installIdentityVal  ChecksumIdentity
)

// InstallChecksumIdentity returns the stable per-install machine id pair, creating
// and persisting it on first use.
func InstallChecksumIdentity() ChecksumIdentity {
	installIdentityOnce.Do(func() {
		installIdentityVal = loadOrCreateInstallIdentity(appdata.MachineIDFilePath(), appdata.MacMachineIDFilePath())
	})
	return installIdentityVal
}

// InstallMachineID returns just the per-install serviceMachineId.
func InstallMachineID() string {
	return InstallChecksumIdentity().MachineID
}

func loadOrCreateInstallIdentity(machinePath, macPath string) ChecksumIdentity {
	return ChecksumIdentity{
		MachineID:    loadOrCreateID(machinePath, uuid.NewString),
		MacMachineID: loadOrCreateID(macPath, func() string { return randomHex(32) }),
	}
}

// loadOrCreateID reads a persisted id from path, or generates one with gen and
// writes it. A write failure only costs persistence: the process keeps the value
// in memory, so the checksum stays stable until the next restart.
func loadOrCreateID(path string, gen func() string) string {
	path = strings.TrimSpace(path)
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}
	id := gen()
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
		}
	}
	return id
}

// randomHex returns a lowercase hex string of n random bytes (2*n characters),
// matching Cursor's sha256-style macMachineId. On the near-impossible rand
// failure it degrades to uuid-derived entropy.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		fallback := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
		for len(fallback) < n*2 {
			fallback += strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		return fallback[:n*2]
	}
	return hex.EncodeToString(buf)
}
