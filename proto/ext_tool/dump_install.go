package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cursorInstallSource is one JavaScript bundle that contains protobuf-es descriptors.
type cursorInstallSource struct {
	Version string
	Path    string
	Count   int
}

// findCursorProtoSource picks the installed Cursor bundle with the most
// makeMessageType descriptors. Cursor 3.30.14 keeps agent.v1 and aiserver.v1
// in workbench.desktop.main.js as makeMessageType("pkg.Msg", () => [fields]).
func findCursorProtoSource(installRoot string) (cursorInstallSource, error) {
	installRoot = strings.TrimSpace(installRoot)
	if installRoot == "" {
		return cursorInstallSource{}, fmt.Errorf("cursor install path is required")
	}
	product := filepath.Join(installRoot, "resources", "app", "product.json")
	version := readCursorVersion(product)
	app := filepath.Join(installRoot, "resources", "app")
	var best cursorInstallSource
	err := filepath.WalkDir(app, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".js") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() < 100_000 || info.Size() > 80_000_000 {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(body)
		count := strings.Count(text, ".makeMessageType(")
		if count < best.Count {
			return nil
		}
		if !strings.Contains(text, "agent.v1.ExecServerMessage") || !strings.Contains(text, "aiserver.v1.AvailableModelsRequest") {
			return nil
		}
		best = cursorInstallSource{Version: version, Path: path, Count: count}
		return nil
	})
	if err != nil {
		return cursorInstallSource{}, err
	}
	if best.Path == "" {
		return cursorInstallSource{}, fmt.Errorf("no Cursor protobuf bundle under %s", installRoot)
	}
	return best, nil
}

func readCursorVersion(productPath string) string {
	body, err := os.ReadFile(productPath)
	if err != nil {
		return ""
	}
	text := string(body)
	for _, key := range []string{`"version": "`, `"version":"`} {
		last := strings.LastIndex(text, key)
		if last < 0 {
			continue
		}
		rest := text[last+len(key):]
		end := strings.IndexByte(rest, '"')
		if end > 0 {
			return rest[:end]
		}
	}
	return ""
}
