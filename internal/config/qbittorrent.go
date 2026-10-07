package config

import "lernae/internal/acquisition"

// Non-secret configuration is still private: endpoints and usernames are not
// exposed in public JSON, status output, or accidental formatted diagnostics.
type QBittorrentSettings struct {
	Enabled  bool   `json:"-"`
	BaseURL  string `json:"-"`
	Username string `json:"-"`
}

func (QBittorrentSettings) String() string     { return "QBittorrentSettings{values:<private>}" }
func (s QBittorrentSettings) GoString() string { return s.String() }

type QBittorrentCredentials struct {
	Password string `json:"-"`
}

func (c QBittorrentCredentials) HasCredentials() bool {
	return acquisition.ValidQBittorrentCredential(c.Password)
}
func (QBittorrentCredentials) String() string     { return "QBittorrentCredentials{password:<redacted>}" }
func (c QBittorrentCredentials) GoString() string { return c.String() }

type qbittorrentSettingsDocument struct {
	Enabled  bool   `json:"enabled"`
	BaseURL  string `json:"base_url,omitempty"`
	Username string `json:"username,omitempty"`
}

func ValidateQBittorrentSettings(settings QBittorrentSettings) error {
	if settings.BaseURL != "" {
		if _, err := acquisition.NormalizeQBittorrentBaseURL(settings.BaseURL); err != nil {
			return ErrInvalidRuntimeSettings
		}
	}
	if settings.Username != "" && !acquisition.ValidQBittorrentCredential(settings.Username) {
		return ErrInvalidRuntimeSettings
	}
	if settings.Enabled && (settings.BaseURL == "" || settings.Username == "") {
		return ErrInvalidRuntimeSettings
	}
	return nil
}

func normalizeQBittorrentSettings(settings QBittorrentSettings) QBittorrentSettings {
	if settings.BaseURL != "" {
		settings.BaseURL, _ = acquisition.NormalizeQBittorrentBaseURL(settings.BaseURL)
	}
	return settings
}

func nonEmptyQBittorrentSettings(s QBittorrentSettings) *qbittorrentSettingsDocument {
	if s == (QBittorrentSettings{}) {
		return nil
	}
	s = normalizeQBittorrentSettings(s)
	return &qbittorrentSettingsDocument{Enabled: s.Enabled, BaseURL: s.BaseURL, Username: s.Username}
}

func (store *FileProviderConfigStore) SaveQBittorrent(credentials QBittorrentCredentials) error {
	if !credentials.HasCredentials() {
		return ErrProviderCredentialsIncomplete
	}
	return store.update(func(c *ProviderConfig) { c.QBittorrent = credentials })
}

func (store *FileProviderConfigStore) ClearQBittorrent() error {
	return store.update(func(c *ProviderConfig) { c.QBittorrent = QBittorrentCredentials{} })
}

func decodeQBittorrentCredentials(raw []byte) (QBittorrentCredentials, bool) {
	fields, ok := decodeProviderFields(raw)
	if !ok {
		return QBittorrentCredentials{}, false
	}
	password, ok := decodeRequiredString(fields, "password")
	if !ok {
		return QBittorrentCredentials{}, false
	}
	credentials := QBittorrentCredentials{Password: password}
	return credentials, credentials.HasCredentials()
}

func loadQBittorrentSettings(settings QBittorrentSettings) (QBittorrentSettings, QBittorrentCredentials, error) {
	if ValidateQBittorrentSettings(settings) != nil {
		return QBittorrentSettings{}, QBittorrentCredentials{}, ErrInvalidRuntimeSettings
	}
	settings = normalizeQBittorrentSettings(settings)
	if !settings.Enabled {
		return settings, QBittorrentCredentials{}, nil
	}
	store, err := NewDefaultFileProviderConfigStore()
	if err != nil {
		return QBittorrentSettings{}, QBittorrentCredentials{}, ErrProviderConfigUnavailable
	}
	providers, err := store.Load()
	if err != nil {
		return QBittorrentSettings{}, QBittorrentCredentials{}, ErrProviderConfigUnavailable
	}
	return settings, providers.QBittorrent, nil
}
