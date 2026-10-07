package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const RuntimeSettingsVersion = 1

type MetadataLanguage string

const (
	MetadataLanguageEnglish  MetadataLanguage = "en"
	MetadataLanguageSpanish  MetadataLanguage = "es"
	MetadataLanguageOriginal MetadataLanguage = "original"
)

func (language MetadataLanguage) Valid() bool {
	switch language {
	case MetadataLanguageEnglish, MetadataLanguageSpanish, MetadataLanguageOriginal:
		return true
	default:
		return false
	}
}

func EffectiveMetadataLanguage(language MetadataLanguage) MetadataLanguage {
	if language == "" {
		return MetadataLanguageEnglish
	}
	return language
}

var (
	ErrRuntimeSettingsPath        = errors.New("runtime settings path must be absolute")
	ErrRuntimeSettingsRead        = errors.New("runtime settings could not be read safely")
	ErrRuntimeSettingsWrite       = errors.New("runtime settings could not be saved safely")
	ErrRuntimeSettingsSymlink     = errors.New("runtime settings must not be a symbolic link")
	ErrRuntimeSettingsNotRegular  = errors.New("runtime settings must be a regular file")
	ErrRuntimeSettingsPermissions = errors.New("runtime settings permissions are unsafe")
	ErrRuntimeSettingsOwner       = errors.New("runtime settings are not owned by the current user")
	ErrRuntimeSettingsMalformed   = errors.New("runtime settings are malformed or have an unsupported schema")
	ErrRuntimeSettingsVersion     = errors.New("runtime settings version is unsupported")
	ErrRuntimeSettingsLock        = errors.New("runtime settings could not be locked safely")
	ErrRuntimeSettingsRepository  = errors.New("runtime settings path must be outside a Git repository")
)

// RuntimeSettings contains only non-secret, explicitly configured values.
// Empty fields remain unset so callers can resolve safe defaults separately.
type RuntimeSettings struct {
	Version          int                      `json:"-"`
	Server           ServerSettings           `json:"server,omitempty"`
	Agent            AgentSettings            `json:"agent,omitempty"`
	Inventory        InventorySettings        `json:"inventory,omitempty"`
	Metadata         MetadataSettings         `json:"metadata,omitempty"`
	Wikidata         WikidataSettings         `json:"wikidata,omitempty"`
	LocalAcquisition LocalAcquisitionSettings `json:"-"`
	Prowlarr         ProwlarrSettings         `json:"-"`
	QBittorrent      QBittorrentSettings      `json:"-"`
}

// LocalAcquisitionSettings is persisted privately by the store's allowlist,
// never included in accidental public JSON or formatted diagnostics.
type LocalAcquisitionSettings struct {
	Enabled     bool   `json:"-"`
	SourceRoot  string `json:"-"`
	StagingRoot string `json:"-"`
}

func (LocalAcquisitionSettings) String() string     { return "LocalAcquisitionSettings{roots:<private>}" }
func (s LocalAcquisitionSettings) GoString() string { return s.String() }

type localAcquisitionDocument struct {
	Enabled     bool   `json:"enabled"`
	SourceRoot  string `json:"source_root,omitempty"`
	StagingRoot string `json:"staging_root,omitempty"`
}

type ServerSettings struct {
	DatabasePath  string `json:"database_path,omitempty"`
	ListenAddress string `json:"listen_address,omitempty"`
}

type AgentSettings struct {
	SocketPath  string `json:"socket_path,omitempty"`
	CachePath   string `json:"cache_path,omitempty"`
	StagingPath string `json:"staging_path,omitempty"`
}

type InventorySettings struct {
	Source       string `json:"source,omitempty"`
	ManifestPath string `json:"manifest_path,omitempty"`
}

type MetadataSettings struct {
	Language MetadataLanguage `json:"language,omitempty"`
}

type WikidataSettings struct {
	ContactEmail string `json:"contact_email,omitempty"`
}

var runtimeSettingsPathLocks sync.Map

// FileRuntimeSettingsStore stores allowlisted non-secret settings in the
// versioned XDG config document. Provider credentials are never represented.
type FileRuntimeSettingsStore struct {
	path   string
	rename func(oldPath, newPath string) error
}

// SettingsConfigPath resolves an explicit XDG config home or the standard
// ~/.config fallback. Explicit roots keep tests independent of real HOME/XDG.
func SettingsConfigPath(xdgConfigHome, home string) (string, error) {
	root := xdgConfigHome
	if root == "" {
		if !filepath.IsAbs(home) {
			return "", ErrRuntimeSettingsPath
		}
		root = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(root) {
		return "", ErrRuntimeSettingsPath
	}
	return filepath.Join(filepath.Clean(root), "lernae", "settings.json"), nil
}

func NewFileRuntimeSettingsStore(xdgConfigHome, home string) (*FileRuntimeSettingsStore, error) {
	path, err := SettingsConfigPath(xdgConfigHome, home)
	if err != nil {
		return nil, err
	}
	path, err = canonicalProviderConfigTarget(path)
	if err != nil {
		return nil, runtimeSettingsError(err, ErrRuntimeSettingsRepository)
	}
	return &FileRuntimeSettingsStore{path: path, rename: os.Rename}, nil
}

// NewDefaultFileRuntimeSettingsStore resolves standard config roots for
// production callers. Tests should use NewFileRuntimeSettingsStore instead.
func NewDefaultFileRuntimeSettingsStore() (*FileRuntimeSettingsStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ErrRuntimeSettingsPath
	}
	return NewFileRuntimeSettingsStore(os.Getenv("XDG_CONFIG_HOME"), home)
}

func (store *FileRuntimeSettingsStore) Load() (RuntimeSettings, error) {
	if store == nil || store.path == "" {
		return RuntimeSettings{}, ErrRuntimeSettingsPath
	}
	return store.load()
}

// Save replaces the settings document atomically. Use Update for independent
// field changes so concurrent read-modify-write operations are not lost.
func (store *FileRuntimeSettingsStore) Save(settings RuntimeSettings) error {
	return store.Update(func(current *RuntimeSettings) { *current = settings })
}

// Update applies one change while holding both process-local and cross-process
// locks, preserving independent updates across store instances.
func (store *FileRuntimeSettingsStore) Update(change func(*RuntimeSettings)) error {
	if store == nil || store.path == "" || change == nil {
		return ErrRuntimeSettingsPath
	}
	if err := store.validateRepositoryPath(); err != nil {
		return err
	}
	if err := ensurePrivateProviderConfigDirectory(filepath.Dir(store.path)); err != nil {
		return runtimeSettingsError(err, ErrRuntimeSettingsWrite)
	}
	unlockPath := lockRuntimeSettingsPath(store.path)
	defer unlockPath()
	unlockFile, err := acquireProviderConfigFileLock(store.path)
	if err != nil {
		return runtimeSettingsError(err, ErrRuntimeSettingsLock)
	}
	defer unlockFile()

	settings, err := store.load()
	if err != nil {
		return err
	}
	change(&settings)
	settings.Version = RuntimeSettingsVersion
	return store.write(settings)
}

func (store *FileRuntimeSettingsStore) load() (RuntimeSettings, error) {
	if err := store.validateRepositoryPath(); err != nil {
		return RuntimeSettings{}, err
	}
	file, err := openProviderConfigFile(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyRuntimeSettings(), nil
		}
		return RuntimeSettings{}, runtimeSettingsError(err, ErrRuntimeSettingsRead)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return RuntimeSettings{}, ErrRuntimeSettingsRead
	}
	if err := validateSettingsFileDirectory(filepath.Dir(store.path)); err != nil {
		return RuntimeSettings{}, err
	}
	if err := validateSettingsFileInfo(info); err != nil {
		return RuntimeSettings{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeSettingsBytes+1))
	if err != nil || len(data) > maxRuntimeSettingsBytes {
		return RuntimeSettings{}, ErrRuntimeSettingsRead
	}
	return decodeRuntimeSettings(data)
}

func (store *FileRuntimeSettingsStore) write(settings RuntimeSettings) error {
	if err := store.validateRepositoryPath(); err != nil {
		return err
	}
	if err := ensurePrivateProviderConfigDirectory(filepath.Dir(store.path)); err != nil {
		return runtimeSettingsError(err, ErrRuntimeSettingsWrite)
	}
	if err := validateSettingsTarget(store.path); err != nil {
		return err
	}
	data, err := encodeRuntimeSettings(settings)
	if err != nil {
		return ErrRuntimeSettingsWrite
	}
	temp, err := os.CreateTemp(filepath.Dir(store.path), ".settings-*.tmp")
	if err != nil {
		return ErrRuntimeSettingsWrite
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return ErrRuntimeSettingsWrite
	}
	written, err := temp.Write(data)
	if err != nil || written != len(data) {
		_ = temp.Close()
		return ErrRuntimeSettingsWrite
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return ErrRuntimeSettingsWrite
	}
	if err := temp.Close(); err != nil {
		return ErrRuntimeSettingsWrite
	}
	if err := validateSettingsTarget(store.path); err != nil {
		return err
	}
	if err := store.validateRepositoryPath(); err != nil {
		return err
	}
	replace := store.rename
	if replace == nil {
		replace = os.Rename
	}
	if err := replace(tempPath, store.path); err != nil {
		return ErrRuntimeSettingsWrite
	}
	keepTemp = false
	return nil
}

func emptyRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{Version: RuntimeSettingsVersion}
}

type runtimeSettingsDocument struct {
	Version  int             `json:"version"`
	Settings json.RawMessage `json:"settings"`
}

type runtimeSettingsPayload struct {
	Server           *ServerSettings              `json:"server,omitempty"`
	Agent            *AgentSettings               `json:"agent,omitempty"`
	Inventory        *InventorySettings           `json:"inventory,omitempty"`
	Metadata         *MetadataSettings            `json:"metadata,omitempty"`
	Wikidata         *WikidataSettings            `json:"wikidata,omitempty"`
	LocalAcquisition *localAcquisitionDocument    `json:"local_acquisition,omitempty"`
	Prowlarr         *prowlarrSettingsDocument    `json:"prowlarr,omitempty"`
	QBittorrent      *qbittorrentSettingsDocument `json:"qbittorrent,omitempty"`
}

func decodeRuntimeSettings(data []byte) (RuntimeSettings, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	if len(top) != 2 || top["version"] == nil || top["settings"] == nil {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	if err := validateRuntimeSettingsShape(top["settings"]); err != nil {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	var document runtimeSettingsDocument
	if err := decodeSettingsJSON(data, &document); err != nil {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	if document.Version != 0 && document.Version != RuntimeSettingsVersion {
		return RuntimeSettings{}, ErrRuntimeSettingsVersion
	}
	trimmedSettings := bytes.TrimSpace(document.Settings)
	if len(trimmedSettings) == 0 || trimmedSettings[0] != '{' {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	var payload runtimeSettingsPayload
	if err := decodeSettingsJSON(trimmedSettings, &payload); err != nil {
		return RuntimeSettings{}, ErrRuntimeSettingsMalformed
	}
	settings := RuntimeSettings{}
	if payload.Server != nil {
		settings.Server = *payload.Server
	}
	if payload.Agent != nil {
		settings.Agent = *payload.Agent
	}
	if payload.Inventory != nil {
		settings.Inventory = *payload.Inventory
	}
	if payload.Metadata != nil {
		settings.Metadata = *payload.Metadata
	}
	if payload.Wikidata != nil {
		settings.Wikidata = *payload.Wikidata
	}
	if payload.LocalAcquisition != nil {
		settings.LocalAcquisition = LocalAcquisitionSettings{Enabled: payload.LocalAcquisition.Enabled, SourceRoot: payload.LocalAcquisition.SourceRoot, StagingRoot: payload.LocalAcquisition.StagingRoot}
	}
	if payload.QBittorrent != nil {
		settings.QBittorrent = QBittorrentSettings{Enabled: payload.QBittorrent.Enabled, BaseURL: payload.QBittorrent.BaseURL, Username: payload.QBittorrent.Username}
		if ValidateQBittorrentSettings(settings.QBittorrent) != nil {
			return RuntimeSettings{}, ErrInvalidRuntimeSettings
		}
		settings.QBittorrent = normalizeQBittorrentSettings(settings.QBittorrent)
	}
	if payload.Prowlarr != nil {
		settings.Prowlarr = ProwlarrSettings{Enabled: payload.Prowlarr.Enabled, BaseURL: payload.Prowlarr.BaseURL}
		if ValidateProwlarrSettings(settings.Prowlarr) != nil {
			return RuntimeSettings{}, ErrInvalidRuntimeSettings
		}
		settings.Prowlarr = normalizeProwlarrSettings(settings.Prowlarr)
	}
	if ValidateLocalAcquisitionSettings(settings.LocalAcquisition) != nil {
		return RuntimeSettings{}, ErrInvalidRuntimeSettings
	}
	settings.Version = RuntimeSettingsVersion
	return settings, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrRuntimeSettingsMalformed
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrRuntimeSettingsMalformed
			}
			if _, exists := keys[key]; exists {
				return ErrRuntimeSettingsMalformed
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrRuntimeSettingsMalformed
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrRuntimeSettingsMalformed
		}
	default:
		return ErrRuntimeSettingsMalformed
	}
	return nil
}

func validateRuntimeSettingsShape(raw json.RawMessage) error {
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil || sections == nil {
		return ErrRuntimeSettingsMalformed
	}
	allowedSections := map[string]map[string]struct{}{
		"server":            {"database_path": {}, "listen_address": {}},
		"agent":             {"socket_path": {}, "cache_path": {}, "staging_path": {}},
		"inventory":         {"source": {}, "manifest_path": {}},
		"metadata":          {"language": {}},
		"wikidata":          {"contact_email": {}},
		"local_acquisition": {"enabled": {}, "source_root": {}, "staging_root": {}},
		"prowlarr":          {"enabled": {}, "base_url": {}},
		"qbittorrent":       {"enabled": {}, "base_url": {}, "username": {}},
	}
	for section, fields := range sections {
		allowedFields, ok := allowedSections[section]
		if !ok {
			return ErrRuntimeSettingsMalformed
		}
		trimmedFields := bytes.TrimSpace(fields)
		if len(trimmedFields) == 0 || trimmedFields[0] != '{' {
			return ErrRuntimeSettingsMalformed
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(fields, &values); err != nil || values == nil {
			return ErrRuntimeSettingsMalformed
		}
		for name, rawValue := range values {
			if _, ok := allowedFields[name]; !ok {
				return ErrRuntimeSettingsMalformed
			}
			trimmedValue := bytes.TrimSpace(rawValue)
			if (section == "local_acquisition" || section == "prowlarr" || section == "qbittorrent") && name == "enabled" {
				if !bytes.Equal(trimmedValue, []byte("true")) && !bytes.Equal(trimmedValue, []byte("false")) {
					return ErrRuntimeSettingsMalformed
				}
				continue
			}
			if len(trimmedValue) == 0 || trimmedValue[0] != '"' {
				return ErrRuntimeSettingsMalformed
			}
		}
	}
	return nil
}

func decodeSettingsJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrRuntimeSettingsMalformed
	}
	return nil
}

func encodeRuntimeSettings(settings RuntimeSettings) ([]byte, error) {
	if err := ValidateQBittorrentSettings(settings.QBittorrent); err != nil {
		return nil, err
	}
	if err := ValidateProwlarrSettings(settings.Prowlarr); err != nil {
		return nil, err
	}
	if err := ValidateLocalAcquisitionSettings(settings.LocalAcquisition); err != nil {
		return nil, err
	}
	document := struct {
		Version  int                    `json:"version"`
		Settings runtimeSettingsPayload `json:"settings"`
	}{
		Version: RuntimeSettingsVersion,
		Settings: runtimeSettingsPayload{
			Server:           nonEmptyServerSettings(settings.Server),
			Agent:            nonEmptyAgentSettings(settings.Agent),
			Inventory:        nonEmptyInventorySettings(settings.Inventory),
			Metadata:         nonEmptyMetadataSettings(settings.Metadata),
			Wikidata:         nonEmptyWikidataSettings(settings.Wikidata),
			LocalAcquisition: nonEmptyLocalAcquisitionSettings(settings.LocalAcquisition),
			Prowlarr:         nonEmptyProwlarrSettings(settings.Prowlarr),
			QBittorrent:      nonEmptyQBittorrentSettings(settings.QBittorrent),
		},
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, ErrRuntimeSettingsWrite
	}
	return append(data, '\n'), nil
}

func nonEmptyLocalAcquisitionSettings(s LocalAcquisitionSettings) *localAcquisitionDocument {
	if s == (LocalAcquisitionSettings{}) {
		return nil
	}
	return &localAcquisitionDocument{Enabled: s.Enabled, SourceRoot: s.SourceRoot, StagingRoot: s.StagingRoot}
}

func nonEmptyMetadataSettings(settings MetadataSettings) *MetadataSettings {
	if settings == (MetadataSettings{}) {
		return nil
	}
	return &settings
}

func nonEmptyWikidataSettings(settings WikidataSettings) *WikidataSettings {
	if settings == (WikidataSettings{}) {
		return nil
	}
	return &settings
}

func nonEmptyServerSettings(settings ServerSettings) *ServerSettings {
	if settings == (ServerSettings{}) {
		return nil
	}
	return &settings
}

func nonEmptyAgentSettings(settings AgentSettings) *AgentSettings {
	if settings == (AgentSettings{}) {
		return nil
	}
	return &settings
}

func nonEmptyInventorySettings(settings InventorySettings) *InventorySettings {
	if settings == (InventorySettings{}) {
		return nil
	}
	return &settings
}

const maxRuntimeSettingsBytes = 1 << 20

func validateSettingsFileDirectory(path string) error {
	if err := validateProviderConfigDirectory(path); err != nil {
		return runtimeSettingsError(err, ErrRuntimeSettingsRead)
	}
	return nil
}

func validateSettingsTarget(path string) error {
	if err := validateProviderConfigTarget(path); err != nil {
		return runtimeSettingsError(err, ErrRuntimeSettingsWrite)
	}
	return nil
}

func validateSettingsFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return ErrRuntimeSettingsNotRegular
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return ErrRuntimeSettingsPermissions
	}
	if !providerConfigOwnedByCurrentUser(info) {
		return ErrRuntimeSettingsOwner
	}
	return nil
}

func (store *FileRuntimeSettingsStore) validateRepositoryPath() error {
	if store == nil || store.path == "" {
		return ErrRuntimeSettingsPath
	}
	canonical, err := canonicalProviderConfigTarget(store.path)
	if err != nil {
		return runtimeSettingsError(err, ErrRuntimeSettingsRepository)
	}
	if canonical != store.path {
		return ErrRuntimeSettingsRepository
	}
	return nil
}

func lockRuntimeSettingsPath(path string) func() {
	key := canonicalProviderConfigPath(path)
	value, _ := runtimeSettingsPathLocks.LoadOrStore(key, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func runtimeSettingsError(err, fallback error) error {
	switch {
	case errors.Is(err, ErrProviderConfigPath):
		return ErrRuntimeSettingsPath
	case errors.Is(err, ErrProviderConfigSymlink):
		return ErrRuntimeSettingsSymlink
	case errors.Is(err, ErrProviderConfigNotRegular):
		return ErrRuntimeSettingsNotRegular
	case errors.Is(err, ErrProviderConfigPermissions):
		return ErrRuntimeSettingsPermissions
	case errors.Is(err, ErrProviderConfigOwner):
		return ErrRuntimeSettingsOwner
	case errors.Is(err, ErrProviderConfigRepository):
		return ErrRuntimeSettingsRepository
	case errors.Is(err, ErrProviderConfigLock):
		return ErrRuntimeSettingsLock
	default:
		return fallback
	}
}
