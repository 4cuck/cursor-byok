package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The app opens more than one Store on the same config file, each with its own
// lock. Saves from two of them must not share a temporary file: one renaming
// what the other is still writing fails the save ("cannot find the file",
// "being used by another process") or puts a half-written config in place.

func configTempFiles(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read config directory: %v", err)
	}
	var temps []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".tmp") {
			temps = append(temps, entry.Name())
		}
	}
	return temps
}

func TestTwoStoresSavingTheSameConfigDoNotCollide(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	stores := []*Store{NewStore(path, ""), NewStore(path, "")}
	const savesPerStore = 40

	failures := make(chan error, len(stores)*savesPerStore)
	var writers sync.WaitGroup
	for index, store := range stores {
		writers.Add(1)
		go func() {
			defer writers.Done()
			cfg := DefaultConfig()
			cfg.Log = index == 0
			for range savesPerStore {
				if _, err := store.Save(ctx, cfg); err != nil {
					failures <- err
				}
			}
		}()
	}
	writers.Wait()
	close(failures)

	for err := range failures {
		t.Errorf("Save() error = %v", err)
	}
	if _, err := stores[0].Load(ctx); err != nil {
		t.Fatalf("the config no longer loads: %v", err)
	}
	if left := configTempFiles(t, path); len(left) != 0 {
		t.Fatalf("temporary config files left behind: %v", left)
	}
}

// A save that crashed leaves its temporary file behind, holding the same
// credentials as the config. The next save clears it; a temporary file another
// save is still writing is left alone.
func TestSaveClearsTemporaryFilesOfCrashedSaves(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	crashed := path + ".tmp-crashed"
	inFlight := path + ".tmp-inflight"
	for _, temp := range []string{crashed, inFlight} {
		if err := os.WriteFile(temp, []byte("apiKey: secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	longAgo := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(crashed, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path, "").Save(ctx, DefaultConfig()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := os.Stat(crashed); !os.IsNotExist(err) {
		t.Fatalf("temporary file of a crashed save is still there: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("temporary file of a save in progress was removed: %v", err)
	}
}
