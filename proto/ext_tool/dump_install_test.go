package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindCursorProtoSourceReadsInstalledVersion(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "resources", "app")
	bundle := filepath.Join(app, "out", "vs", "workbench", "workbench.desktop.main.js")
	if err := os.MkdirAll(filepath.Dir(bundle), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":"9.9.9"}`
	if err := os.WriteFile(filepath.Join(app, "product.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	js := stringsRepeat(`Da.makeMessageType("agent.v1.ExecServerMessage",(()=>[{no:1,name:"id",kind:"scalar",T:13}]));`, 1) +
		`Da.makeMessageType("aiserver.v1.AvailableModelsRequest",(()=>[{no:1,name:"is_nightly",kind:"scalar",T:8}]));`
	padding := make([]byte, 100_000)
	for i := range padding {
		padding[i] = ' '
	}
	if err := os.WriteFile(bundle, append([]byte(js), padding...), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := findCursorProtoSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if source.Version != "9.9.9" || source.Count != 2 || source.Path != bundle {
		t.Fatalf("source = %+v", source)
	}
}

func stringsRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
