package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"cursor/internal/logger"
)

const (
	// configFilePerm / configDirPerm 只允许当前用户读写：用户配置里保存了
	// 模型供应商 apiKey 与余额查询凭据，不能对同机其他用户可读。
	configFilePerm os.FileMode = 0o600
	configDirPerm  os.FileMode = 0o700
)

type Store struct {
	path     string
	logsRoot string
	mu       sync.Mutex
}

type fileSnapshot struct {
	exists  bool
	modTime int64
	size    int64
}

func NewStore(path string, logsRoot string) *Store {
	return &Store{
		path:     strings.TrimSpace(path),
		logsRoot: strings.TrimSpace(logsRoot),
	}
}

func (store *Store) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

func (store *Store) snapshot() fileSnapshot {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return fileSnapshot{}
	}
	info, err := os.Stat(store.path)
	if err != nil {
		return fileSnapshot{}
	}
	return fileSnapshot{
		exists:  true,
		modTime: info.ModTime().UnixNano(),
		size:    info.Size(),
	}
}

func (store *Store) Load(_ context.Context) (Config, error) {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return DefaultConfig(), nil
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	restrictExistingPermissions(store.path)

	data, err := os.ReadFile(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			defaultConfig := DefaultConfig()
			if err := store.saveLocked(defaultConfig); err != nil {
				return DefaultConfig(), err
			}
			return defaultConfig, nil
		}
		return DefaultConfig(), fmt.Errorf("读取用户配置失败: %w", err)
	}

	var current Config
	if err := yaml.Unmarshal(data, &current); err != nil {
		return DefaultConfig(), fmt.Errorf("解析用户配置失败: %w", err)
	}
	hasDelegation := yamlHasKey(data, "delegation")
	hasDelegationEnabled := yamlHasNestedKey(data, "delegation", "enabled")
	if !hasDelegation || !hasDelegationEnabled {
		current.Delegation.Enabled = DefaultConfig().Delegation.Enabled
	}
	if hasDelegationEnabled && !yamlHasNestedKey(data, "delegation", "maxConcurrency") {
		current.Delegation.MaxConcurrency = DefaultDelegationMaxConcurrency
	}
	// billingQuery 默认开启：旧配置文件未出现该键（或未写 enabled 子键）时按默认值处理，
	// 避免 Go 零值 false 把存量用户的计费查询静默关闭。
	if !yamlHasKey(data, "billingQuery") || !yamlHasNestedKey(data, "billingQuery", "enabled") {
		current.BillingQuery = BillingQueryConfig{Enabled: DefaultBillingQueryEnabled}
	}
	normalized, err := NormalizeConfig(current)
	if err != nil {
		return DefaultConfig(), err
	}
	if shouldPersistNormalizedConfig(data, current, normalized) {
		if err := store.saveLocked(normalized); err != nil {
			return DefaultConfig(), err
		}
	}
	return normalized, nil
}

func (store *Store) Save(_ context.Context, cfg Config) (Config, error) {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return Config{}, errors.New("配置存储未初始化")
	}

	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return Config{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := store.saveLocked(normalized); err != nil {
		return Config{}, err
	}
	return normalized, nil
}

func (store *Store) saveLocked(normalized Config) error {
	if err := os.MkdirAll(filepath.Dir(store.path), configDirPerm); err != nil {
		return fmt.Errorf("创建用户配置目录失败: %w", err)
	}

	data, err := yaml.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("序列化用户配置失败: %w", err)
	}

	// Each save writes a temporary file of its own. The app opens several
	// Stores on this path, each with its own lock, and a shared temporary file
	// lets one of them rename what another is still writing.
	removeStaleConfigTemps(store.path)
	tempPath, err := writeConfigTemp(store.path, data)
	if err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := os.Chmod(tempPath, configFilePerm); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("设置临时配置权限失败: %w", err)
	}
	if err := replaceConfigFile(tempPath, store.path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("保存用户配置失败: %w", err)
	}
	return nil
}

// configTempPattern names the temporary files of saves to path.
func configTempPattern(path string) string {
	return filepath.Base(path) + ".tmp-*"
}

// writeConfigTemp writes data to a new temporary file next to path and
// returns its name. The file is created readable by the current user only.
func writeConfigTemp(path string, data []byte) (string, error) {
	temp, err := os.CreateTemp(filepath.Dir(path), configTempPattern(path))
	if err != nil {
		return "", err
	}
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(temp.Name())
		return "", err
	}
	return temp.Name(), nil
}

// staleConfigTempAge is how old a temporary config file must be before it is
// taken for the leftover of a crashed save. A save finishes within seconds.
const staleConfigTempAge = time.Minute

// removeStaleConfigTemps deletes temporary files that crashed saves left next
// to path. They hold the same credentials as the config itself.
func removeStaleConfigTemps(path string) {
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), configTempPattern(path)))
	if err != nil {
		return
	}
	for _, leftover := range leftovers {
		if info, err := os.Stat(leftover); err == nil && time.Since(info.ModTime()) > staleConfigTempAge {
			_ = os.Remove(leftover)
		}
	}
}

// replaceConfigFile moves tempPath over path.
//
// Windows refuses the move with "Access is denied" while anything has the
// destination open: a virus scanner reading the file that was just written,
// or another reader of the config. Such handles close within milliseconds, so
// the move is retried briefly before the save is reported as failed. Other
// systems replace an open file without complaint and get a single attempt.
func replaceConfigFile(tempPath string, path string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 12
	}
	delay := 10 * time.Millisecond
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = os.Rename(tempPath, path); err == nil || os.IsNotExist(err) || attempt == attempts {
			return err
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
	return err
}

// restrictExistingPermissions 收紧已存在配置文件与所在目录的权限。
// 配置里含 apiKey / balanceAccessToken 等凭据，早期版本以 0644 落盘，
// 升级后首次读取时就地迁移，避免同机其他用户可读。
func restrictExistingPermissions(path string) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return
	}
	if dir := filepath.Dir(trimmed); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() && info.Mode().Perm() != configDirPerm {
			if err := os.Chmod(dir, configDirPerm); err != nil {
				logger.Warn("收紧用户配置目录权限失败", "error", err)
			}
		}
	}
	info, err := os.Stat(trimmed)
	if err != nil || info.IsDir() || info.Mode().Perm() == configFilePerm {
		return
	}
	if err := os.Chmod(trimmed, configFilePerm); err != nil {
		logger.Warn("收紧用户配置文件权限失败", "error", err)
	}
}

func shouldPersistNormalizedConfig(raw []byte, current Config, normalized Config) bool {
	if yamlHasKey(raw, "routing") {
		return true
	}
	if !yamlHasKey(raw, "backendListenAddr") || !yamlHasKey(raw, "proxyListenAddr") {
		return true
	}
	if current.BackendListenAddr != normalized.BackendListenAddr || current.ProxyListenAddr != normalized.ProxyListenAddr {
		return true
	}
	if current.ProviderStreamIdleTimeout != normalized.ProviderStreamIdleTimeout && yamlHasKey(raw, "providerStreamIdleTimeout") {
		return true
	}
	if current.TurnStaleTimeout != normalized.TurnStaleTimeout && yamlHasKey(raw, "turnStaleTimeout") {
		return true
	}
	if current.NativeDelegationProgressTimeout != normalized.NativeDelegationProgressTimeout && yamlHasKey(raw, "nativeDelegationProgressTimeout") {
		return true
	}
	if modelAdapterIDsChanged(current.ModelAdapters, normalized.ModelAdapters) {
		return true
	}
	if !reflect.DeepEqual(current.Delegation, normalized.Delegation) {
		return true
	}
	if !reflect.DeepEqual(current.MCPTrustGrants, normalized.MCPTrustGrants) {
		return true
	}
	if !yamlHasKey(raw, "billingQuery") || !yamlHasNestedKey(raw, "billingQuery", "enabled") {
		return true
	}
	return false
}

func modelAdapterIDsChanged(current []ModelAdapterConfig, normalized []ModelAdapterConfig) bool {
	if len(current) != len(normalized) {
		return true
	}
	for index := range current {
		if strings.TrimSpace(current[index].ID) != strings.TrimSpace(normalized[index].ID) {
			return true
		}
	}
	return false
}

func yamlHasKey(raw []byte, key string) bool {
	return yamlMappingHasKey(yamlRootMapping(raw), key)
}

func yamlHasNestedKey(raw []byte, parent string, key string) bool {
	mapping := yamlRootMapping(raw)
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == parent {
			return yamlMappingHasKey(mapping.Content[index+1], key)
		}
	}
	return false
}

func yamlRootMapping(raw []byte) *yaml.Node {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return &yaml.Node{}
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return &yaml.Node{}
	}
	return root.Content[0]
}

func yamlMappingHasKey(mapping *yaml.Node, key string) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return true
		}
	}
	return false
}
