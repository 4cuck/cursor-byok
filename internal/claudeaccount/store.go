package claudeaccount

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// storeFileName is the accounts file inside the app's data directory.
	storeFileName = "claude-accounts.json"

	// The file holds access tokens, so only the current user may read it.
	storeDirPerm  os.FileMode = 0o700
	storeFilePerm os.FileMode = 0o600
)

// record is one saved account as it is stored, token included.
type record struct {
	ID               string   `json:"id"`
	AccessToken      string   `json:"accessToken"`
	ExpiresAtUnixMS  int64    `json:"expiresAtUnixMs,omitempty"`
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    string   `json:"rateLimitTier,omitempty"`
	CreatedAtUnixMS  int64    `json:"createdAtUnixMs,omitempty"`
	UpdatedAtUnixMS  int64    `json:"updatedAtUnixMs,omitempty"`

	// Profile and Usage are what Anthropic last reported, kept so the account
	// can be shown without asking again.
	Profile *Profile `json:"profile,omitempty"`
	Usage   *Usage   `json:"usage,omitempty"`
	// UsageAtUnixMS is when Usage was last read.
	UsageAtUnixMS int64 `json:"usageAtUnixMs,omitempty"`
	// DetailsError is why the last attempt to read Profile and Usage did not
	// get both, or "".
	DetailsError string `json:"detailsError,omitempty"`
	// Rejected is whether Anthropic refused the token the last time it was
	// used here.
	Rejected bool `json:"rejected,omitempty"`
}

type storeDocument struct {
	Accounts []record `json:"accounts"`
}

// store reads and writes the accounts file. It does no locking of its own;
// Manager serializes access.
type store struct {
	path string
}

// load returns the saved accounts. A file that does not exist holds none.
func (s *store) load() ([]record, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, wrapCoded(CodeStoreFailed, "the Claude accounts file could not be read", err)
	}
	var document storeDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, wrapCoded(CodeStoreFailed, "the Claude accounts file is damaged", err)
	}
	return document.Accounts, nil
}

// save replaces the accounts file with records. The new content is written to
// a temporary file first and then moved into place, so a crash leaves either
// the old file or the new one.
func (s *store) save(records []record) error {
	if records == nil {
		records = []record{}
	}
	data, err := json.MarshalIndent(storeDocument{Accounts: records}, "", "  ")
	if err != nil {
		return wrapCoded(CodeStoreFailed, "the Claude accounts could not be encoded", err)
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, storeDirPerm); err != nil {
		return wrapCoded(CodeStoreFailed, "the data directory could not be created", err)
	}
	temp, err := os.CreateTemp(directory, storeFileName+".tmp-*")
	if err != nil {
		return wrapCoded(CodeStoreFailed, "the Claude accounts file could not be written", err)
	}
	tempPath := temp.Name()
	_, writeErr := temp.Write(append(data, '\n'))
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tempPath)
		return wrapCoded(CodeStoreFailed, "the Claude accounts file could not be written", err)
	}
	if err := os.Chmod(tempPath, storeFilePerm); err != nil {
		_ = os.Remove(tempPath)
		return wrapCoded(CodeStoreFailed, "the Claude accounts file could not be protected", err)
	}
	if err := replaceFile(tempPath, s.path); err != nil {
		_ = os.Remove(tempPath)
		return wrapCoded(CodeStoreFailed, "the Claude accounts file could not be saved", err)
	}
	return nil
}

// replaceFile moves tempPath over path. Windows refuses the move while
// anything, such as a virus scanner, has the destination open; such handles
// close within milliseconds, so the move is retried briefly there.
func replaceFile(tempPath string, path string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 10
	}
	delay := 10 * time.Millisecond
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = os.Rename(tempPath, path); err == nil || attempt == attempts {
			return err
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
	return err
}
