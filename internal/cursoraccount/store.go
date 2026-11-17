package cursoraccount

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	storeDirName     = "cursor-accounts"
	accountsDirName  = "accounts"
	indexFileName    = "index.json"
	currentFileName  = "current.json"
	legacyDirName    = "legacy"
	legacyBackupName = "cursor-account.json.bak"

	storeDirPerm  os.FileMode = 0o700
	storeFilePerm os.FileMode = 0o600
)

// CursorAccountSummary is a non-secret account view. It must never include
// access tokens, refresh tokens, cookies, or raw credential JSON.
type CursorAccountSummary struct {
	ID               string   `json:"id"`
	Email            string   `json:"email,omitempty"`
	AuthIDHint       string   `json:"authIdHint,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	IsCurrent        bool     `json:"isCurrent"`
	LastUsedAtUnixMS int64    `json:"lastUsedAtUnixMs,omitempty"`
	// Lock state. Cursor caps how many distinct devices may use one account
	// within 24 hours; once tripped, requests fail with "Too many computers"
	// and keep failing until the window rolls over.
	LockedUntilUnixMS int64  `json:"lockedUntilUnixMs,omitempty"`
	LockReason        string `json:"lockReason,omitempty"`
}

// AccountStore is the restricted on-disk account library under <root>/cursor-accounts/.
type AccountStore struct {
	root       string
	legacyPath string
	dir        string
	mu         sync.Mutex
}

type accountRecord struct {
	ID           string   `json:"id"`
	Email        string   `json:"email,omitempty"`
	AuthID       string   `json:"authId,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	CreatedAt    int64    `json:"createdAt,omitempty"`
	LastUsedAt   int64    `json:"lastUsedAtUnixMs,omitempty"`
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	// Lock state, persisted so a restart does not reset the cooldown.
	LockedUntilUnixMS int64  `json:"lockedUntilUnixMs,omitempty"`
	LockReason        string `json:"lockReason,omitempty"`
	LockedAtUnixMS    int64  `json:"lockedAtUnixMs,omitempty"`
}

type accountIndex struct {
	Accounts []indexEntry `json:"accounts"`
}

type indexEntry struct {
	ID               string   `json:"id"`
	Email            string   `json:"email,omitempty"`
	AuthIDHint       string   `json:"authIdHint,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	CreatedAt        int64    `json:"createdAt,omitempty"`
	LastUsedAtUnixMS int64    `json:"lastUsedAtUnixMs,omitempty"`
	// Mirrored into the index so List() can report lock state without opening
	// every account file.
	LockedUntilUnixMS int64  `json:"lockedUntilUnixMs,omitempty"`
	LockReason        string `json:"lockReason,omitempty"`
}

type currentPointer struct {
	ID string `json:"id"`
}

func NewAccountStore(root, legacyPath string) *AccountStore {
	root = strings.TrimSpace(root)
	return &AccountStore{
		root:       root,
		legacyPath: strings.TrimSpace(legacyPath),
		dir:        filepath.Join(root, storeDirName),
	}
}

func (s *AccountStore) IndexPathForTest() string {
	if s == nil {
		return ""
	}
	return s.indexPath()
}

func (s *AccountStore) indexPath() string {
	return filepath.Join(s.dir, indexFileName)
}

func (s *AccountStore) currentPath() string {
	return filepath.Join(s.dir, currentFileName)
}

func (s *AccountStore) accountsDir() string {
	return filepath.Join(s.dir, accountsDirName)
}

func (s *AccountStore) List() ([]CursorAccountSummary, error) {
	if s == nil {
		return []CursorAccountSummary{}, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return nil, err
	}
	return s.listLocked()
}

func (s *AccountStore) Upsert(value credentials) (CursorAccountSummary, error) {
	if s == nil {
		return CursorAccountSummary{}, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return CursorAccountSummary{}, err
	}
	return s.upsertLocked(value)
}

func (s *AccountStore) LoadCurrentCredentials() (credentials, string, error) {
	if s == nil {
		return credentials{}, "", fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return credentials{}, "", err
	}
	currentID, err := s.readCurrentIDLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, "", nil
		}
		return credentials{}, "", err
	}
	if currentID == "" {
		return credentials{}, "", nil
	}
	record, err := s.readAccountLocked(currentID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, "", nil
		}
		return credentials{}, "", err
	}
	return credentials{
		AccessToken:  strings.TrimSpace(record.AccessToken),
		RefreshToken: strings.TrimSpace(record.RefreshToken),
		AuthID:       strings.TrimSpace(record.AuthID),
		Email:        strings.TrimSpace(record.Email),
	}, record.ID, nil
}

func (s *AccountStore) SetCurrent(id string) (CursorAccountSummary, error) {
	if s == nil {
		return CursorAccountSummary{}, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return CursorAccountSummary{}, err
	}
	return s.setCurrentLocked(id)
}

func (s *AccountStore) UpdateCredentials(id string, value credentials) error {
	if s == nil {
		return fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return err
	}
	return s.updateCredentialsLocked(id, value)
}

func (s *AccountStore) UpdateTags(id string, tags []string) (CursorAccountSummary, error) {
	if s == nil {
		return CursorAccountSummary{}, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return CursorAccountSummary{}, err
	}
	return s.updateTagsLocked(id, tags)
}

// MarkAccountLocked records that Cursor refused this account for device-count
// reasons. The cooldown is persisted so restarting the app does not let the
// user hammer the account again immediately.
func (s *AccountStore) MarkAccountLocked(id, reason string, until time.Time) (CursorAccountSummary, error) {
	if s == nil {
		return CursorAccountSummary{}, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return CursorAccountSummary{}, err
	}
	return s.setLockLocked(id, reason, until.UnixMilli())
}

// ClearAccountLock removes the cooldown. Used by the explicit "try anyway"
// action and when a request finally succeeds.
func (s *AccountStore) ClearAccountLock(id string) (CursorAccountSummary, error) {
	if s == nil {
		return CursorAccountSummary{}, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return CursorAccountSummary{}, err
	}
	return s.setLockLocked(id, "", 0)
}

// setLockLocked mutates the lock fields on the record, the account file and the
// index entry, so List() and LoadCurrentCredentials() stay consistent.
// setLockLocked mutates the lock fields on the account file and its index entry,
// so List() and LoadCurrentCredentials() stay consistent.
func (s *AccountStore) setLockLocked(id, reason string, untilUnixMS int64) (CursorAccountSummary, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return CursorAccountSummary{}, fmt.Errorf("account id is empty")
	}
	record, err := s.readAccountLocked(id)
	if err != nil {
		return CursorAccountSummary{}, err
	}
	record.LockReason = strings.TrimSpace(reason)
	record.LockedUntilUnixMS = untilUnixMS
	if untilUnixMS > 0 {
		record.LockedAtUnixMS = time.Now().UnixMilli()
	} else {
		record.LockedAtUnixMS = 0
	}
	if err := s.replaceRecordLocked(record); err != nil {
		return CursorAccountSummary{}, err
	}
	records, err := s.loadRecordsLocked()
	if err != nil {
		return CursorAccountSummary{}, err
	}
	if err := s.writeIndexLocked(records); err != nil {
		return CursorAccountSummary{}, err
	}
	currentID, _ := s.readCurrentIDLocked()
	return recordToSummary(record, currentID == id), nil
}

func (s *AccountStore) Delete(req CursorAccountDeleteRequest) error {
	if s == nil {
		return fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return err
	}
	return s.deleteLocked(req)
}

func (s *AccountStore) recordsByIDs(ids []string) ([]accountRecord, error) {
	if s == nil {
		return nil, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return nil, err
	}
	return s.recordsByIDsLocked(ids)
}

func (s *AccountStore) clearCurrent() error {
	if s == nil {
		return fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return err
	}
	_, err := os.Stat(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeJSONFile(s.currentPath(), currentPointer{ID: ""})
}

func (s *AccountStore) CurrentAccountID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.readCurrentIDLocked()
	if err != nil {
		return ""
	}
	return id
}

func (s *AccountStore) needsMigration() bool {
	_, dirErr := os.Stat(s.dir)
	_, currentErr := os.Stat(s.currentPath())
	return errors.Is(dirErr, os.ErrNotExist) && errors.Is(currentErr, os.ErrNotExist)
}

func (s *AccountStore) migrateIfNeededLocked() error {
	if s.root == "" {
		return fmt.Errorf("account store root is empty")
	}
	if s.needsMigration() {
		if err := s.commitLegacyMigrationLocked(); err != nil {
			return err
		}
	}
	if s.storeDirExists() {
		if moveErr := s.moveLegacyBackup(); moveErr != nil {
			// Store commit already succeeded. A leftover legacy file is retried
			// on the next List/load and must not fail current-credential reads.
		}
	}
	return nil
}

func (s *AccountStore) storeDirExists() bool {
	_, err := os.Stat(s.dir)
	return err == nil
}

func (s *AccountStore) commitLegacyMigrationLocked() error {
	creds, err := s.readLegacyCredentials()
	if err != nil {
		return err
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil
	}
	now := time.Now().UnixMilli()
	record := accountRecord{
		ID:           uuid.NewString(),
		Email:        strings.TrimSpace(creds.Email),
		AuthID:       strings.TrimSpace(creds.AuthID),
		Tags:         []string{},
		CreatedAt:    now,
		LastUsedAt:   now,
		AccessToken:  strings.TrimSpace(creds.AccessToken),
		RefreshToken: strings.TrimSpace(creds.RefreshToken),
	}
	staging := s.dir + ".tmp"
	_ = os.RemoveAll(staging)
	if err := writeStoreTree(staging, []accountRecord{record}, record.ID); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, s.dir); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("commit account store: %w", err)
	}
	return nil
}

func writeStoreTree(dir string, records []accountRecord, currentID string) error {
	if err := os.MkdirAll(filepath.Join(dir, accountsDirName), storeDirPerm); err != nil {
		return err
	}
	entries := make([]indexEntry, 0, len(records))
	for _, record := range records {
		path := filepath.Join(dir, accountsDirName, record.ID+".json")
		if err := writeJSONFile(path, record); err != nil {
			return fmt.Errorf("write account file: %w", err)
		}
		entries = append(entries, indexEntryFromRecord(record))
	}
	if err := writeJSONFile(filepath.Join(dir, indexFileName), accountIndex{Accounts: entries}); err != nil {
		return fmt.Errorf("write account index: %w", err)
	}
	if err := writeJSONFile(filepath.Join(dir, currentFileName), currentPointer{ID: currentID}); err != nil {
		return fmt.Errorf("write current account pointer: %w", err)
	}
	return nil
}

func (s *AccountStore) moveLegacyBackup() error {
	if s.legacyPath == "" {
		return nil
	}
	if _, err := os.Stat(s.legacyPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	backupDir := filepath.Join(s.root, legacyDirName)
	if err := os.MkdirAll(backupDir, storeDirPerm); err != nil {
		return err
	}
	backupPath := filepath.Join(backupDir, legacyBackupName)
	if err := os.Rename(s.legacyPath, backupPath); err != nil {
		return fmt.Errorf("move legacy credentials: %w", err)
	}
	if err := os.Chmod(backupPath, storeFilePerm); err != nil {
		return err
	}
	return nil
}

func (s *AccountStore) readLegacyCredentials() (credentials, error) {
	if s.legacyPath == "" {
		return credentials{}, nil
	}
	data, err := os.ReadFile(s.legacyPath)
	if errors.Is(err, os.ErrNotExist) {
		return credentials{}, nil
	}
	if err != nil {
		return credentials{}, fmt.Errorf("read legacy credentials: %w", err)
	}
	loaded := credentials{}
	if err := json.Unmarshal(data, &loaded); err != nil {
		return credentials{}, fmt.Errorf("parse legacy credentials: %w", err)
	}
	loaded.AccessToken = strings.TrimSpace(loaded.AccessToken)
	loaded.RefreshToken = strings.TrimSpace(loaded.RefreshToken)
	loaded.AuthID = strings.TrimSpace(loaded.AuthID)
	loaded.Email = strings.TrimSpace(loaded.Email)
	return loaded, nil
}

func (s *AccountStore) listLocked() ([]CursorAccountSummary, error) {
	currentID, err := s.readCurrentIDLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entries, err := s.loadIndexEntriesLocked()
	if err != nil {
		return nil, err
	}
	summaries := make([]CursorAccountSummary, 0, len(entries))
	for _, entry := range entries {
		if err := validateAccountID(entry.ID); err != nil {
			continue
		}
		summaries = append(summaries, summaryFromIndexEntry(entry, entry.ID == currentID))
	}
	return summaries, nil
}

func (s *AccountStore) loadIndexEntriesLocked() ([]indexEntry, error) {
	entries, err := s.readIndexLocked()
	if err == nil {
		return entries, nil
	}
	missing := errors.Is(err, os.ErrNotExist)
	if !missing && !isCorruptIndex(err) {
		return nil, err
	}
	records, scanErr := s.scanAccountFilesLocked()
	if scanErr != nil {
		return nil, fmt.Errorf("rebuild account index: %w", scanErr)
	}
	entries = make([]indexEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, indexEntryFromRecord(record))
	}
	if missing && len(entries) == 0 {
		return entries, nil
	}
	if err := writeJSONFile(s.indexPath(), accountIndex{Accounts: entries}); err != nil {
		return nil, fmt.Errorf("rebuild account index: %w", err)
	}
	return entries, nil
}

func isCorruptIndex(err error) bool {
	var syntax *json.SyntaxError
	var unmarshalType *json.UnmarshalTypeError
	return errors.As(err, &syntax) || errors.As(err, &unmarshalType)
}

func (s *AccountStore) readIndexLocked() ([]indexEntry, error) {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		return nil, err
	}
	index := accountIndex{Accounts: []indexEntry{}}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	if index.Accounts == nil {
		index.Accounts = []indexEntry{}
	}
	return index.Accounts, nil
}

func (s *AccountStore) scanAccountFilesLocked() ([]accountRecord, error) {
	entries, err := os.ReadDir(s.accountsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []accountRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]accountRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if err := validateAccountID(id); err != nil {
			continue
		}
		record, err := s.readAccountLocked(id)
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *AccountStore) upsertLocked(value credentials) (CursorAccountSummary, error) {
	value.AccessToken = strings.TrimSpace(value.AccessToken)
	value.RefreshToken = strings.TrimSpace(value.RefreshToken)
	value.AuthID = strings.TrimSpace(value.AuthID)
	value.Email = strings.TrimSpace(value.Email)
	if value.AccessToken == "" {
		return CursorAccountSummary{}, fmt.Errorf("access token is empty")
	}
	records, err := s.loadRecordsLocked()
	if err != nil {
		return CursorAccountSummary{}, err
	}
	now := time.Now().UnixMilli()
	matched := -1
	matchedByAuthID := false
	if value.AuthID != "" {
		for i, record := range records {
			if strings.TrimSpace(record.AuthID) == value.AuthID {
				matched = i
				matchedByAuthID = true
				break
			}
		}
	} else {
		for i, record := range records {
			// 无 AuthID 的导入（本地 state.vscdb / 官方备份文件）按 email 合并：
			// Cursor 客户端会轮换 token，同账号再次导入时 token 已变，
			// 只认「email+token 双匹配」会让同一账号不断产生重复记录。
			if value.Email != "" && strings.EqualFold(strings.TrimSpace(record.Email), value.Email) {
				matched = i
				break
			}
		}
	}
	if matched >= 0 {
		if value.Email != "" {
			records[matched].Email = value.Email
		}
		if value.AuthID != "" {
			records[matched].AuthID = value.AuthID
		}
		records[matched].AccessToken = value.AccessToken
		if value.RefreshToken != "" && (matchedByAuthID || records[matched].RefreshToken == "") && !keepDurableAPIKey(records[matched].RefreshToken, value.RefreshToken) {
			records[matched].RefreshToken = value.RefreshToken
		}
		records[matched].LastUsedAt = now
		if records[matched].Tags == nil {
			records[matched].Tags = []string{}
		}
	} else {
		records = append(records, accountRecord{
			ID:           uuid.NewString(),
			Email:        value.Email,
			AuthID:       value.AuthID,
			Tags:         []string{},
			CreatedAt:    now,
			LastUsedAt:   now,
			AccessToken:  value.AccessToken,
			RefreshToken: value.RefreshToken,
		})
		matched = len(records) - 1
	}
	if err := s.persistRecordAndIndexLocked(records, matched); err != nil {
		return CursorAccountSummary{}, err
	}
	currentID, currentErr := s.readCurrentIDLocked()
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return CursorAccountSummary{}, currentErr
	}
	return recordToSummary(records[matched], records[matched].ID == currentID), nil
}

func (s *AccountStore) loadRecordsLocked() ([]accountRecord, error) {
	entries, err := s.loadIndexEntriesLocked()
	if err != nil {
		return nil, err
	}
	records := make([]accountRecord, 0, len(entries))
	for _, entry := range entries {
		if err := validateAccountID(entry.ID); err != nil {
			continue
		}
		record, err := s.readAccountLocked(entry.ID)
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		return s.scanAccountFilesLocked()
	}
	return records, nil
}

func (s *AccountStore) persistRecordAndIndexLocked(records []accountRecord, updatedIndex int) error {
	if updatedIndex < 0 || updatedIndex >= len(records) {
		return fmt.Errorf("account record index out of range")
	}
	record := records[updatedIndex]
	path, err := s.accountPath(record.ID)
	if err != nil {
		return err
	}
	if err := writeJSONFile(path, record); err != nil {
		return fmt.Errorf("write account file: %w", err)
	}
	entries := make([]indexEntry, 0, len(records))
	for _, item := range records {
		entries = append(entries, indexEntryFromRecord(item))
	}
	if err := writeJSONFile(s.indexPath(), accountIndex{Accounts: entries}); err != nil {
		return fmt.Errorf("write account index: %w", err)
	}
	return nil
}

func (s *AccountStore) setCurrentLocked(id string) (CursorAccountSummary, error) {
	if err := validateAccountID(id); err != nil {
		return CursorAccountSummary{}, err
	}
	record, err := s.readAccountLocked(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CursorAccountSummary{}, accountError("account_not_found", "account not found")
		}
		return CursorAccountSummary{}, err
	}
	record.LastUsedAt = time.Now().UnixMilli()
	path, err := s.accountPath(record.ID)
	if err != nil {
		return CursorAccountSummary{}, err
	}
	if err := writeJSONFile(path, record); err != nil {
		return CursorAccountSummary{}, fmt.Errorf("write account file: %w", err)
	}
	if err := s.touchIndexLastUsedLocked(record); err != nil {
		return CursorAccountSummary{}, err
	}
	if err := writeJSONFile(s.currentPath(), currentPointer{ID: record.ID}); err != nil {
		return CursorAccountSummary{}, fmt.Errorf("write current account pointer: %w", err)
	}
	return recordToSummary(record, true), nil
}

// keepDurableAPIKey 拒绝用短期 JWT 覆盖已保存的 crsr_ key。
// 那个 key 是唯一能在一小时后续期的凭据。
func keepDurableAPIKey(existing, incoming string) bool {
	return IsCursorAPIKey(existing) && incoming != "" && !IsCursorAPIKey(incoming)
}

func (s *AccountStore) updateCredentialsLocked(id string, value credentials) error {
	if err := validateAccountID(id); err != nil {
		return err
	}
	record, err := s.readAccountLocked(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return accountError("account_not_found", "account not found")
		}
		return err
	}
	value.AccessToken = strings.TrimSpace(value.AccessToken)
	value.RefreshToken = strings.TrimSpace(value.RefreshToken)
	value.AuthID = strings.TrimSpace(value.AuthID)
	value.Email = strings.TrimSpace(value.Email)
	if value.AccessToken == "" {
		return fmt.Errorf("access token is empty")
	}
	record.AccessToken = value.AccessToken
	if value.RefreshToken != "" && !keepDurableAPIKey(record.RefreshToken, value.RefreshToken) {
		record.RefreshToken = value.RefreshToken
	}
	if value.Email != "" {
		record.Email = value.Email
	}
	if value.AuthID != "" {
		record.AuthID = value.AuthID
	}
	record.LastUsedAt = time.Now().UnixMilli()
	return s.replaceRecordLocked(record)
}

type storedCredential struct {
	ID    string
	Creds credentials
}

// accountsForPresentedToken 找出客户端拿来刷新的 token 属于哪个已保存账号。
// 先精确匹配 access/refresh，再匹配 JWT sub 与 AuthID，最后在只有一个
// API Key 账号且出示的 JWT 已过期时回落到那个账号。
func (s *AccountStore) accountsForPresentedToken(presented, subject string) ([]storedCredential, error) {
	if s == nil {
		return nil, fmt.Errorf("account store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateIfNeededLocked(); err != nil {
		return nil, err
	}
	records, err := s.loadRecordsLocked()
	if err != nil {
		return nil, err
	}
	presented = strings.TrimSpace(presented)
	subject = strings.TrimSpace(subject)
	exact := make([]storedCredential, 0, 1)
	bySubject := make([]storedCredential, 0, 1)
	apiKeys := make([]storedCredential, 0, 1)
	for _, record := range records {
		item := storedCredential{
			ID: record.ID,
			Creds: credentials{
				AccessToken:  strings.TrimSpace(record.AccessToken),
				RefreshToken: strings.TrimSpace(record.RefreshToken),
				AuthID:       strings.TrimSpace(record.AuthID),
				Email:        strings.TrimSpace(record.Email),
			},
		}
		if presented != "" && (item.Creds.AccessToken == presented || item.Creds.RefreshToken == presented) {
			exact = append(exact, item)
		}
		if authIDsMatch(item.Creds.AuthID, subject) {
			bySubject = append(bySubject, item)
		}
		if IsCursorAPIKey(item.Creds.RefreshToken) {
			apiKeys = append(apiKeys, item)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	if len(bySubject) > 0 {
		return bySubject, nil
	}
	if len(apiKeys) == 1 && tokenNeedsRefresh(presented, time.Now()) {
		return apiKeys, nil
	}
	return nil, nil
}

func authIDsMatch(stored, subject string) bool {
	stored = strings.TrimSpace(stored)
	subject = strings.TrimSpace(subject)
	if stored == "" || subject == "" {
		return false
	}
	if stored == subject {
		return true
	}
	return strings.HasSuffix(subject, "|"+stored) || strings.HasSuffix(stored, "|"+subject)
}

func (s *AccountStore) updateTagsLocked(id string, tags []string) (CursorAccountSummary, error) {
	if err := validateAccountID(id); err != nil {
		return CursorAccountSummary{}, err
	}
	normalized, err := normalizeTags(tags)
	if err != nil {
		return CursorAccountSummary{}, err
	}
	record, err := s.readAccountLocked(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CursorAccountSummary{}, accountError("account_not_found", "account not found")
		}
		return CursorAccountSummary{}, err
	}
	record.Tags = normalized
	if err := s.replaceRecordLocked(record); err != nil {
		return CursorAccountSummary{}, err
	}
	currentID, currentErr := s.readCurrentIDLocked()
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return CursorAccountSummary{}, currentErr
	}
	return recordToSummary(record, record.ID == currentID), nil
}

func (s *AccountStore) replaceRecordLocked(record accountRecord) error {
	records, err := s.loadRecordsLocked()
	if err != nil {
		return err
	}
	matched := -1
	for i, item := range records {
		if item.ID == record.ID {
			matched = i
			break
		}
	}
	if matched < 0 {
		records = append(records, record)
		matched = len(records) - 1
	} else {
		records[matched] = record
	}
	return s.persistRecordAndIndexLocked(records, matched)
}

func (s *AccountStore) deleteLocked(req CursorAccountDeleteRequest) error {
	ids, err := collectUniqueIDs(req.AccountIDs, 100)
	if err != nil {
		return err
	}
	replacementID := strings.TrimSpace(req.ReplacementID)
	if replacementID != "" && req.ClearCurrent {
		return fmt.Errorf("replacement id and clear current are mutually exclusive")
	}
	if replacementID != "" {
		if err := validateAccountID(replacementID); err != nil {
			return err
		}
	}

	currentID, err := s.readCurrentIDLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	deleting := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		deleting[id] = struct{}{}
		if _, readErr := s.readAccountLocked(id); readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				return accountError("account_not_found", "account not found")
			}
			return readErr
		}
	}

	deletesCurrent := currentID != ""
	if _, ok := deleting[currentID]; !ok {
		deletesCurrent = false
	}
	if deletesCurrent {
		if replacementID == "" && !req.ClearCurrent {
			return fmt.Errorf("deleting the current account requires a replacement or clear current")
		}
	} else if replacementID != "" || req.ClearCurrent {
		return fmt.Errorf("replacement id and clear current are only valid when deleting the current account")
	}
	if replacementID != "" {
		if _, ok := deleting[replacementID]; ok {
			return fmt.Errorf("replacement account cannot be deleted")
		}
		if _, readErr := s.readAccountLocked(replacementID); readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				return accountError("account_not_found", "account not found")
			}
			return readErr
		}
	}

	if replacementID != "" {
		if _, err := s.setCurrentLocked(replacementID); err != nil {
			return err
		}
	} else if req.ClearCurrent || deletesCurrent {
		if err := writeJSONFile(s.currentPath(), currentPointer{ID: ""}); err != nil {
			return err
		}
	}

	records, err := s.loadRecordsLocked()
	if err != nil {
		return err
	}
	kept := make([]accountRecord, 0, len(records))
	for _, record := range records {
		if _, ok := deleting[record.ID]; ok {
			path, pathErr := s.accountPath(record.ID)
			if pathErr != nil {
				return pathErr
			}
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			continue
		}
		kept = append(kept, record)
	}
	return s.writeIndexLocked(kept)
}

func (s *AccountStore) writeIndexLocked(records []accountRecord) error {
	entries := make([]indexEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, indexEntryFromRecord(record))
	}
	return writeJSONFile(s.indexPath(), accountIndex{Accounts: entries})
}

func (s *AccountStore) recordsByIDsLocked(ids []string) ([]accountRecord, error) {
	records := make([]accountRecord, 0, len(ids))
	for _, id := range ids {
		record, err := s.readAccountLocked(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, accountError("account_not_found", "account not found")
			}
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func collectUniqueIDs(ids []string, max int) ([]string, error) {
	if len(ids) == 0 {
		return nil, errAccountIDsEmpty
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errAccountIDEmpty
		}
		if err := validateAccountID(id); err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if max > 0 && len(out) > max {
			return nil, errTooManyAccountIDs
		}
	}
	return out, nil
}

func normalizeTags(tags []string) ([]string, error) {
	const (
		maxTags  = 20
		maxRunes = 32
	)
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > maxRunes {
			return nil, fmt.Errorf("tag exceeds 32 characters")
		}
		key := strings.ToLower(tag)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, tag)
		if len(out) > maxTags {
			return nil, fmt.Errorf("too many tags")
		}
	}
	return out, nil
}

func (s *AccountStore) touchIndexLastUsedLocked(record accountRecord) error {
	entries, err := s.loadIndexEntriesLocked()
	if err != nil {
		return err
	}
	found := false
	for i, entry := range entries {
		if entry.ID == record.ID {
			entries[i] = indexEntryFromRecord(record)
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, indexEntryFromRecord(record))
	}
	if err := writeJSONFile(s.indexPath(), accountIndex{Accounts: entries}); err != nil {
		return fmt.Errorf("write account index: %w", err)
	}
	return nil
}

func (s *AccountStore) readCurrentIDLocked() (string, error) {
	data, err := os.ReadFile(s.currentPath())
	if err != nil {
		return "", err
	}
	pointer := currentPointer{}
	if err := json.Unmarshal(data, &pointer); err != nil {
		return "", fmt.Errorf("parse current account pointer: %w", err)
	}
	id := strings.TrimSpace(pointer.ID)
	if id == "" {
		return "", nil
	}
	if err := validateAccountID(id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *AccountStore) readAccountLocked(id string) (accountRecord, error) {
	path, err := s.accountPath(id)
	if err != nil {
		return accountRecord{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return accountRecord{}, err
	}
	record := accountRecord{}
	if err := json.Unmarshal(data, &record); err != nil {
		return accountRecord{}, fmt.Errorf("parse account %s: %w", id, err)
	}
	record.ID = strings.TrimSpace(record.ID)
	if record.ID == "" {
		record.ID = id
	}
	record.Email = strings.TrimSpace(record.Email)
	record.AuthID = strings.TrimSpace(record.AuthID)
	record.AccessToken = strings.TrimSpace(record.AccessToken)
	record.RefreshToken = strings.TrimSpace(record.RefreshToken)
	if record.Tags == nil {
		record.Tags = []string{}
	}
	return record, nil
}

func (s *AccountStore) accountPath(id string) (string, error) {
	if err := validateAccountID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.accountsDir(), id+".json"), nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), storeDirPerm); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, append(data, '\n'), storeFilePerm); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, storeFilePerm); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return os.Chmod(path, storeFilePerm)
}

func validateAccountID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("account id is empty")
	}
	if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid account id")
	}
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("invalid account id")
		}
	}
	return nil
}

func indexEntryFromRecord(record accountRecord) indexEntry {
	tags := record.Tags
	if tags == nil {
		tags = []string{}
	}
	return indexEntry{
		ID:                record.ID,
		Email:             record.Email,
		AuthIDHint:        record.AuthID,
		Tags:              tags,
		CreatedAt:         record.CreatedAt,
		LastUsedAtUnixMS:  record.LastUsedAt,
		LockedUntilUnixMS: record.LockedUntilUnixMS,
		LockReason:        record.LockReason,
	}
}

func summaryFromIndexEntry(entry indexEntry, isCurrent bool) CursorAccountSummary {
	tags := entry.Tags
	if tags == nil {
		tags = []string{}
	}
	return CursorAccountSummary{
		ID:                entry.ID,
		Email:             entry.Email,
		AuthIDHint:        entry.AuthIDHint,
		Tags:              tags,
		IsCurrent:         isCurrent,
		LastUsedAtUnixMS:  entry.LastUsedAtUnixMS,
		LockedUntilUnixMS: entry.LockedUntilUnixMS,
		LockReason:        entry.LockReason,
	}
}

func recordToSummary(record accountRecord, isCurrent bool) CursorAccountSummary {
	return summaryFromIndexEntry(indexEntryFromRecord(record), isCurrent)
}

var (
	errAccountIDsEmpty   = errors.New("account ids are empty")
	errAccountIDEmpty    = errors.New("account id is empty")
	errTooManyAccountIDs = errors.New("too many account ids")
)
