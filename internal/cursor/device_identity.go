package cursor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cursor/internal/appdata"
	"cursor/internal/logger"
)

// Cursor identifies the host device with storage.serviceMachineId and a
// telemetry.* block in state.vscdb. When cursor-byok injects an account into the
// real Cursor IDE, it overwrites these with the account's own stable random
// device identity so the IDE presents the same single device the proxy does, and
// so the host's real identity is not reused. The host's original values are
// snapshotted once into cursor-device-backup.json and restored by
// RestoreCursorUserInfo, so disconnecting returns the IDE to its own identity.

const (
	cursorStateServiceMachineIDKey = "storage.serviceMachineId"
	cursorTelemetryMachineIDKey    = "telemetry.machineId"
	cursorTelemetryMacMachineIDKey = "telemetry.macMachineId"
	cursorTelemetryDevDeviceIDKey  = "telemetry.devDeviceId"
	cursorTelemetrySqmIDKey        = "telemetry.sqmId"

	cursorDeviceBackupFileName = "cursor-device-backup.json"
)

// cursorDeviceIdentityKeys lists every state.vscdb key cursor-byok overwrites
// when injecting an account's device identity. Order is fixed for stable backups.
var cursorDeviceIdentityKeys = []string{
	cursorStateServiceMachineIDKey,
	cursorTelemetryMachineIDKey,
	cursorTelemetryMacMachineIDKey,
	cursorTelemetryDevDeviceIDKey,
	cursorTelemetrySqmIDKey,
}

// CursorDeviceIdentity is the per-account device identity written into the real
// Cursor state DB. Empty fields are left untouched.
type CursorDeviceIdentity struct {
	ServiceMachineID   string // storage.serviceMachineId (uuid)
	MacMachineID       string // telemetry.macMachineId (64-hex)
	DevDeviceID        string // telemetry.devDeviceId (uuid)
	TelemetryMachineID string // telemetry.machineId (64-hex)
	SqmID              string // telemetry.sqmId ({GUID})
}

// stateValues maps the identity onto its state.vscdb keys, skipping empty fields.
func (identity CursorDeviceIdentity) stateValues() map[string]string {
	out := make(map[string]string, len(cursorDeviceIdentityKeys))
	if v := strings.TrimSpace(identity.ServiceMachineID); v != "" {
		out[cursorStateServiceMachineIDKey] = v
	}
	if v := strings.TrimSpace(identity.MacMachineID); v != "" {
		out[cursorTelemetryMacMachineIDKey] = v
	}
	if v := strings.TrimSpace(identity.DevDeviceID); v != "" {
		out[cursorTelemetryDevDeviceIDKey] = v
	}
	if v := strings.TrimSpace(identity.TelemetryMachineID); v != "" {
		out[cursorTelemetryMachineIDKey] = v
	}
	if v := strings.TrimSpace(identity.SqmID); v != "" {
		out[cursorTelemetrySqmIDKey] = v
	}
	return out
}

// ApplyCursorDeviceIdentity writes the account's device identity into the Cursor
// state DB, snapshotting the host's original values once beforehand. An identity
// with no populated fields is a no-op, so callers that lack one leave the real
// telemetry untouched.
func ApplyCursorDeviceIdentity(path string, identity CursorDeviceIdentity) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("cursor state db path is empty")
	}
	values := identity.stateValues()
	if len(values) == 0 {
		return nil
	}
	if err := backupCursorDeviceIdentity(path); err != nil {
		// A missing backup only costs reversibility of the telemetry block; the
		// account injection itself must still proceed.
		logger.Errorf("backupCursorDeviceIdentity failed: %v", err)
	}
	if err := restoreCursorAuthStateDB(path, values, nil); err != nil {
		return fmt.Errorf("Failed to write the Cursor device identity path=%s: %w", path, err)
	}
	logger.Infof("applyCursorDeviceIdentity synced path=%s keys=%d", path, len(values))
	return nil
}

// cursorDeviceBackupPath returns the host's original device-identity snapshot path.
func cursorDeviceBackupPath() string {
	return filepath.Join(appdata.DataRootPath(), cursorDeviceBackupFileName)
}

// backupCursorDeviceIdentity snapshots the host's original device keys exactly
// once. A nil entry records a key that did not exist, so restore removes it.
func backupCursorDeviceIdentity(stateDBPath string) error {
	backupPath := cursorDeviceBackupPath()
	if _, err := os.Stat(backupPath); err == nil {
		return nil // original identity already captured
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current, err := readCursorStateValues(stateDBPath, cursorDeviceIdentityKeys)
	if err != nil {
		return err
	}
	backup := make(map[string]any, len(cursorDeviceIdentityKeys))
	for _, key := range cursorDeviceIdentityKeys {
		if value, ok := current[key]; ok {
			backup[key] = value
		} else {
			backup[key] = nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(backupPath, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	logger.Infof("backupCursorDeviceIdentity saved path=%s keys=%d", backupPath, len(backup))
	return nil
}

// restoreCursorDeviceIdentity writes the host's original device keys back, or
// removes keys that did not exist originally. It is a no-op when no snapshot
// exists (cursor-byok never injected a device identity on this host).
func restoreCursorDeviceIdentity(stateDBPath string) error {
	backupPath := cursorDeviceBackupPath()
	data, err := os.ReadFile(backupPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var backup map[string]any
	if err := json.Unmarshal(data, &backup); err != nil {
		logger.Infof("restoreCursorDeviceIdentity: backup parse failed, leaving telemetry as-is err=%v", err)
		return nil
	}
	values := map[string]string{}
	removes := []string{}
	for _, key := range cursorDeviceIdentityKeys {
		raw, ok := backup[key]
		if !ok {
			continue
		}
		if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
			values[key] = text
		} else {
			removes = append(removes, key)
		}
	}
	if len(values) == 0 && len(removes) == 0 {
		return nil
	}
	if err := restoreCursorAuthStateDB(stateDBPath, values, removes); err != nil {
		return err
	}
	logger.Infof("restoreCursorDeviceIdentity done path=%s restored=%d removed=%d", stateDBPath, len(values), len(removes))
	return nil
}

// readCursorStateValues reads the given keys from a state DB. Missing keys are
// omitted from the result rather than erroring.
func readCursorStateValues(path string, keys []string) (map[string]string, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	out := make(map[string]string, len(keys))
	for _, key := range keys {
		var raw []byte
		err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key).Scan(&raw)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out[key] = string(raw)
	}
	return out, nil
}
