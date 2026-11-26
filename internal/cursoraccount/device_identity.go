package cursoraccount

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
)

// DeviceIdentity is the per-account set of random device identifiers presented
// to Cursor, both in the proxy's x-cursor-checksum header and (when an account
// is injected into the real Cursor IDE) in state.vscdb. Every field is generated
// once and then kept, so the account always looks like the same single device.
type DeviceIdentity struct {
	// ServiceMachineID is the checksum machineId and storage.serviceMachineId (uuid).
	ServiceMachineID string
	// MacMachineID is the checksum /macMachineId and telemetry.macMachineId (64-hex).
	MacMachineID string
	// DevDeviceID is telemetry.devDeviceId (uuid).
	DevDeviceID string
	// TelemetryMachineID is telemetry.machineId (64-hex).
	TelemetryMachineID string
	// SqmID is telemetry.sqmId ({GUID}).
	SqmID string
}

// deviceIdentity projects a stored record's device fields.
func (record accountRecord) deviceIdentity() DeviceIdentity {
	return DeviceIdentity{
		ServiceMachineID:   strings.TrimSpace(record.MachineID),
		MacMachineID:       strings.TrimSpace(record.MacMachineID),
		DevDeviceID:        strings.TrimSpace(record.DevDeviceID),
		TelemetryMachineID: strings.TrimSpace(record.TelemetryMachineID),
		SqmID:              strings.TrimSpace(record.SqmID),
	}
}

// ensureDeviceIdentityFields fills any missing device identifier on the record
// with a freshly generated value and reports whether anything changed. Existing
// values are never regenerated, so an account's device identity is "set once".
func ensureDeviceIdentityFields(record *accountRecord) bool {
	changed := false
	if strings.TrimSpace(record.MachineID) == "" {
		record.MachineID = uuid.NewString()
		changed = true
	}
	if strings.TrimSpace(record.MacMachineID) == "" {
		record.MacMachineID = randomHex(32)
		changed = true
	}
	if strings.TrimSpace(record.DevDeviceID) == "" {
		record.DevDeviceID = uuid.NewString()
		changed = true
	}
	if strings.TrimSpace(record.TelemetryMachineID) == "" {
		record.TelemetryMachineID = randomHex(32)
		changed = true
	}
	if strings.TrimSpace(record.SqmID) == "" {
		record.SqmID = randomSqmID()
		changed = true
	}
	return changed
}

// randomHex returns a lowercase hex string of the given byte length (so the
// resulting string is 2*n characters), matching Cursor's sha256-style ids. On
// the near-impossible rand failure it degrades to uuid-derived entropy.
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

// randomSqmID returns a Windows-style SQM id: an upper-case GUID in braces.
func randomSqmID() string {
	return "{" + strings.ToUpper(uuid.NewString()) + "}"
}
