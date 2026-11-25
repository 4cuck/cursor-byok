package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// On Windows a file cannot be replaced while anything has it open. A virus
// scanner looking at the config, or another reader of it, holds it for a few
// milliseconds. A save that lands in that window has to wait it out: failing
// with "Access is denied" loses the change the user just made.
func TestSaveSucceedsWhileTheConfigIsBrieflyOpenElsewhere(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewStore(path, "")
	if _, err := store.Save(ctx, DefaultConfig()); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	reader, err := os.Open(path)
	if err != nil {
		t.Fatalf("open config as another reader: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = reader.Close()
		close(released)
	}()

	changed := DefaultConfig()
	changed.Log = true
	_, saveErr := store.Save(ctx, changed)
	<-released

	if saveErr != nil {
		t.Fatalf("Save() while the config was open elsewhere: %v", saveErr)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !loaded.Log {
		t.Fatal("the saved change is not in the file")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary config file left behind: %v", err)
	}
}
