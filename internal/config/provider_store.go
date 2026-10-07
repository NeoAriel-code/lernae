package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const ProviderConfigVersion = 1

var (
	ErrProviderConfigPath            = errors.New("provider config path must be absolute")
	ErrProviderConfigRead            = errors.New("provider config could not be read safely")
	ErrProviderConfigWrite           = errors.New("provider config could not be saved safely")
	ErrProviderConfigSymlink         = errors.New("provider config must not be a symbolic link")
	ErrProviderConfigNotRegular      = errors.New("provider config must be a regular file")
	ErrProviderConfigPermissions     = errors.New("provider config permissions are unsafe")
	ErrProviderConfigOwner           = errors.New("provider config is not owned by the current user")
	ErrProviderConfigMalformed       = errors.New("provider config is malformed or has an unsupported schema")
	ErrProviderConfigVersion         = errors.New("provider config version is unsupported")
	ErrProviderConfigLock            = errors.New("provider config could not be locked safely")
	ErrProviderConfigRepository      = errors.New("provider config path must be outside a Git repository")
	ErrProviderCredentialsIncomplete = errors.New("provider credentials are incomplete")
)

var providerConfigPathLocks sync.Map

// ProviderConfig contains the allowlisted credential bundles used by current
// providers. Its JSON representation is intentionally empty and its formatted
// representation contains only configured-state booleans.
type ProviderConfig struct {
	Version     int                    `json:"-"`
	IGDB        IGDBCredentials        `json:"-"`
	RomM        RomMCredentials        `json:"-"`
	Prowlarr    ProwlarrCredentials    `json:"-"`
	QBittorrent QBittorrentCredentials `json:"-"`
}

func (config ProviderConfig) String() string {
	return "ProviderConfig{igdb_configured:" + configuredValue(config.IGDB.HasCredentials()) +
		", romm_configured:" + configuredValue(config.RomM.HasCredentials()) +
		", prowlarr_configured:" + configuredValue(config.Prowlarr.HasCredentials()) +
		", qbittorrent_password_present:" + configuredValue(config.QBittorrent.HasCredentials()) + "}"
}

func (config ProviderConfig) GoString() string {
	return config.String()
}

// ProviderConfigStore is the small consumer-facing seam shared by replaceable
// credential backends. Callers receive resolved bundles, not backend details.
type ProviderConfigStore interface {
	Load() (ProviderConfig, error)
	SaveIGDB(IGDBCredentials) error
	SaveRomM(RomMCredentials) error
	SaveProwlarr(ProwlarrCredentials) error
	ClearProwlarr() error
}

// FileProviderConfigStore persists the supported provider bundles in the
// versioned XDG config file. Unknown providers and fields are deliberately
// ignored and dropped on the next write; only the current allowlisted schema
// is retained so opaque values cannot accidentally enter formatted output.
type FileProviderConfigStore struct {
	path   string
	rename func(oldPath, newPath string) error
}

var _ ProviderConfigStore = (*FileProviderConfigStore)(nil)

// ProviderConfigPath resolves an explicit XDG config home or the standard
// ~/.config fallback. The explicit arguments keep tests independent of the
// machine's actual HOME and XDG environment.
func ProviderConfigPath(xdgConfigHome, home string) (string, error) {
	root := xdgConfigHome
	if root == "" {
		if !filepath.IsAbs(home) {
			return "", ErrProviderConfigPath
		}
		root = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(root) {
		return "", ErrProviderConfigPath
	}
	return filepath.Join(filepath.Clean(root), "lernae", "providers.json"), nil
}

// NewFileProviderConfigStore creates a file-store adapter using explicit
// roots. The config directory and file are created privately on first save.
func NewFileProviderConfigStore(xdgConfigHome, home string) (*FileProviderConfigStore, error) {
	path, err := ProviderConfigPath(xdgConfigHome, home)
	if err != nil {
		return nil, err
	}
	path, err = canonicalProviderConfigTarget(path)
	if err != nil {
		return nil, err
	}
	return &FileProviderConfigStore{path: path, rename: os.Rename}, nil
}

// NewDefaultFileProviderConfigStore resolves XDG_CONFIG_HOME and the process
// home for production callers. Tests should use NewFileProviderConfigStore
// with temporary roots instead.
func NewDefaultFileProviderConfigStore() (*FileProviderConfigStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ErrProviderConfigPath
	}
	return NewFileProviderConfigStore(os.Getenv("XDG_CONFIG_HOME"), home)
}

func (store *FileProviderConfigStore) Load() (ProviderConfig, error) {
	if store == nil || store.path == "" {
		return ProviderConfig{}, ErrProviderConfigPath
	}
	return store.load()
}

func (store *FileProviderConfigStore) SaveIGDB(credentials IGDBCredentials) error {
	if !credentials.HasCredentials() {
		return ErrProviderCredentialsIncomplete
	}
	return store.update(func(config *ProviderConfig) { config.IGDB = credentials })
}

func (store *FileProviderConfigStore) SaveRomM(credentials RomMCredentials) error {
	if !credentials.HasCredentials() {
		return ErrProviderCredentialsIncomplete
	}
	return store.update(func(config *ProviderConfig) { config.RomM = credentials })
}

func (store *FileProviderConfigStore) update(change func(*ProviderConfig)) error {
	if store == nil || store.path == "" {
		return ErrProviderConfigPath
	}
	if err := store.validateRepositoryPath(); err != nil {
		return err
	}
	configDirectory := filepath.Dir(store.path)
	if err := ensurePrivateProviderConfigDirectory(configDirectory); err != nil {
		return err
	}
	unlockPath := lockProviderConfigPath(store.path)
	defer unlockPath()
	unlockFile, err := acquireProviderConfigFileLock(store.path)
	if err != nil {
		return err
	}
	defer unlockFile()

	config, err := store.load()
	if err != nil {
		return err
	}
	change(&config)
	return store.write(config)
}

func (store *FileProviderConfigStore) load() (ProviderConfig, error) {
	if err := store.validateRepositoryPath(); err != nil {
		return ProviderConfig{}, err
	}
	file, err := openProviderConfigFile(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyProviderConfig(), nil
		}
		if errors.Is(err, ErrProviderConfigSymlink) {
			return ProviderConfig{}, ErrProviderConfigSymlink
		}
		return ProviderConfig{}, ErrProviderConfigRead
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return ProviderConfig{}, ErrProviderConfigRead
	}
	if err := validateProviderConfigDirectory(filepath.Dir(store.path)); err != nil {
		return ProviderConfig{}, err
	}
	if err := validateProviderConfigFileInfo(info); err != nil {
		return ProviderConfig{}, err
	}

	data, err := io.ReadAll(io.LimitReader(file, maxProviderConfigBytes+1))
	if err != nil || len(data) > maxProviderConfigBytes {
		return ProviderConfig{}, ErrProviderConfigRead
	}
	return decodeProviderConfig(data)
}

func (store *FileProviderConfigStore) write(config ProviderConfig) error {
	if err := store.validateRepositoryPath(); err != nil {
		return err
	}
	if err := ensurePrivateProviderConfigDirectory(filepath.Dir(store.path)); err != nil {
		return err
	}
	if err := validateProviderConfigTarget(store.path); err != nil {
		return err
	}
	data, err := encodeProviderConfig(config)
	if err != nil {
		return ErrProviderConfigWrite
	}
	temp, err := os.CreateTemp(filepath.Dir(store.path), ".providers-*.tmp")
	if err != nil {
		return ErrProviderConfigWrite
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
		return ErrProviderConfigWrite
	}
	written, err := temp.Write(data)
	if err != nil || written != len(data) {
		_ = temp.Close()
		return ErrProviderConfigWrite
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return ErrProviderConfigWrite
	}
	if err := temp.Close(); err != nil {
		return ErrProviderConfigWrite
	}
	if err := validateProviderConfigTarget(store.path); err != nil {
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
		return ErrProviderConfigWrite
	}
	keepTemp = false
	return nil
}

func emptyProviderConfig() ProviderConfig {
	return ProviderConfig{Version: ProviderConfigVersion}
}

func configuredValue(configured bool) string {
	if configured {
		return "true"
	}
	return "false"
}

type providerConfigDocument struct {
	Version   int                        `json:"version"`
	Providers map[string]json.RawMessage `json:"providers"`
}

func decodeProviderConfig(data []byte) (ProviderConfig, error) {
	if rejectDuplicateJSONKeys(data) != nil {
		return ProviderConfig{}, ErrProviderConfigMalformed
	}
	var document providerConfigDocument
	if err := json.Unmarshal(data, &document); err != nil || document.Providers == nil {
		return ProviderConfig{}, ErrProviderConfigMalformed
	}
	if document.Version != ProviderConfigVersion {
		return ProviderConfig{}, ErrProviderConfigVersion
	}
	config := emptyProviderConfig()
	for provider, raw := range document.Providers {
		switch provider {
		case "igdb":
			credentials, ok := decodeIGDBCredentials(raw)
			if !ok {
				return ProviderConfig{}, ErrProviderConfigMalformed
			}
			config.IGDB = credentials
		case "qbittorrent":
			credentials, ok := decodeQBittorrentCredentials(raw)
			if !ok {
				return ProviderConfig{}, ErrProviderConfigMalformed
			}
			config.QBittorrent = credentials
		case "prowlarr":
			credentials, ok := decodeProwlarrCredentials(raw)
			if !ok {
				return ProviderConfig{}, ErrProviderConfigMalformed
			}
			config.Prowlarr = credentials
		case "romm":
			credentials, ok := decodeRomMCredentials(raw)
			if !ok {
				return ProviderConfig{}, ErrProviderConfigMalformed
			}
			config.RomM = credentials
		}
	}
	return config, nil
}

func decodeIGDBCredentials(raw json.RawMessage) (IGDBCredentials, bool) {
	fields, ok := decodeProviderFields(raw)
	if !ok {
		return IGDBCredentials{}, false
	}
	clientID, idOK := decodeRequiredString(fields, "client_id")
	clientSecret, secretOK := decodeRequiredString(fields, "client_secret")
	if !idOK || !secretOK {
		return IGDBCredentials{}, false
	}
	credentials := IGDBCredentials{ClientID: clientID, ClientSecret: clientSecret}
	return credentials, credentials.HasCredentials()
}

func decodeRomMCredentials(raw json.RawMessage) (RomMCredentials, bool) {
	fields, ok := decodeProviderFields(raw)
	if !ok {
		return RomMCredentials{}, false
	}
	baseURL, urlOK := decodeRequiredString(fields, "base_url")
	clientToken, tokenOK := decodeRequiredString(fields, "client_api_token")
	if !urlOK || !tokenOK {
		return RomMCredentials{}, false
	}
	credentials := RomMCredentials{BaseURL: baseURL, ClientAPIToken: clientToken}
	return credentials, credentials.HasCredentials()
}

func decodeProviderFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

func decodeRequiredString(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func encodeProviderConfig(config ProviderConfig) ([]byte, error) {
	providers := make(map[string]any, 4)
	if config.QBittorrent.HasCredentials() {
		providers["qbittorrent"] = map[string]string{"password": config.QBittorrent.Password}
	}
	if config.Prowlarr.HasCredentials() {
		providers["prowlarr"] = map[string]string{"api_key": config.Prowlarr.APIKey}
	}
	if config.IGDB.HasCredentials() {
		providers["igdb"] = map[string]string{
			"client_id":     config.IGDB.ClientID,
			"client_secret": config.IGDB.ClientSecret,
		}
	}
	if config.RomM.HasCredentials() {
		providers["romm"] = map[string]string{
			"base_url":         config.RomM.BaseURL,
			"client_api_token": config.RomM.ClientAPIToken,
		}
	}
	document := struct {
		Version   int            `json:"version"`
		Providers map[string]any `json:"providers"`
	}{Version: ProviderConfigVersion, Providers: providers}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, ErrProviderConfigWrite
	}
	return append(data, '\n'), nil
}

const maxProviderConfigBytes = 1 << 20

func ensurePrivateProviderConfigDirectory(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return ErrProviderConfigWrite
		}
		err = os.Mkdir(path, 0o700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return ErrProviderConfigWrite
		}
	} else if err != nil {
		return ErrProviderConfigWrite
	}
	return validateProviderConfigDirectory(path)
}

func validateProviderConfigDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return ErrProviderConfigRead
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrProviderConfigSymlink
	}
	if !info.IsDir() {
		return ErrProviderConfigNotRegular
	}
	if info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return ErrProviderConfigPermissions
	}
	if !providerConfigOwnedByCurrentUser(info) {
		return ErrProviderConfigOwner
	}
	return nil
}

func validateProviderConfigTarget(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrProviderConfigWrite
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrProviderConfigSymlink
	}
	return validateProviderConfigFileInfo(info)
}

func validateProviderConfigFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return ErrProviderConfigNotRegular
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return ErrProviderConfigPermissions
	}
	if !providerConfigOwnedByCurrentUser(info) {
		return ErrProviderConfigOwner
	}
	return nil
}

func validateProviderConfigLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return ErrProviderConfigLock
	}
	if err := validateProviderConfigFileInfo(info); err != nil {
		return err
	}
	return nil
}

func lockProviderConfigPath(path string) func() {
	key := canonicalProviderConfigPath(path)
	value, _ := providerConfigPathLocks.LoadOrStore(key, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (store *FileProviderConfigStore) validateRepositoryPath() error {
	if store == nil || store.path == "" {
		return ErrProviderConfigPath
	}
	canonical, err := canonicalProviderConfigTarget(store.path)
	if errors.Is(err, ErrProviderConfigSymlink) {
		return ErrProviderConfigSymlink
	}
	if err != nil || canonical != store.path {
		return ErrProviderConfigRepository
	}
	return nil
}

// canonicalProviderConfigTarget resolves existing symlink ancestors without
// requiring the config directory to exist, then rejects any target beneath a
// Git repository marker. A failed or ambiguous filesystem check fails closed.
func canonicalProviderConfigTarget(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", ErrProviderConfigRepository
	}
	directory, err := canonicalProviderConfigDirectory(filepath.Dir(absolute))
	if err != nil {
		return "", ErrProviderConfigRepository
	}
	insideRepository, err := providerConfigPathHasGitMarker(directory)
	if err != nil || insideRepository {
		return "", ErrProviderConfigRepository
	}
	if info, err := os.Lstat(filepath.Dir(absolute)); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", ErrProviderConfigSymlink
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", ErrProviderConfigRepository
	}
	return filepath.Join(directory, filepath.Base(absolute)), nil
}

func canonicalProviderConfigDirectory(path string) (string, error) {
	current := filepath.Clean(path)
	missingComponents := make([]string, 0, 4)
	resolveCurrent := func() (string, error) {
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", ErrProviderConfigRepository
		}
		for i := len(missingComponents) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, missingComponents[i])
		}
		return filepath.Clean(resolved), nil
	}
	for {
		resolved, err := resolveCurrent()
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", ErrProviderConfigRepository
		}
		// A dangling symlink is not an ordinary missing component. Do not
		// reconstruct its unresolved path, since that could hide its target.
		// Retry once if the path appeared between resolution and Lstat; this
		// also avoids rejecting directories created concurrently by another
		// store instance.
		if _, lstatErr := os.Lstat(current); lstatErr == nil {
			if resolved, retryErr := resolveCurrent(); retryErr == nil {
				return resolved, nil
			}
			return "", ErrProviderConfigRepository
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return "", ErrProviderConfigRepository
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", ErrProviderConfigRepository
		}
		missingComponents = append(missingComponents, filepath.Base(current))
		current = parent
	}
}

func providerConfigPathHasGitMarker(directory string) (bool, error) {
	for current := filepath.Clean(directory); ; current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, ErrProviderConfigRepository
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
	}
}

func canonicalProviderConfigPath(path string) string {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Clean(path)
	}
	if directory, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		return filepath.Join(directory, filepath.Base(absolute))
	}
	return absolute
}
