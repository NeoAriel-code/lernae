package config

import (
	"path/filepath"

	"lernae/internal/acquisition"
)

// Endpoint is persisted privately in runtime settings, never public diagnostics.
type ProwlarrSettings struct {
	Enabled bool   `json:"-"`
	BaseURL string `json:"-"`
}

func (ProwlarrSettings) String() string     { return "ProwlarrSettings{endpoint:<private>}" }
func (s ProwlarrSettings) GoString() string { return s.String() }

type ProwlarrCredentials struct {
	APIKey string `json:"-"`
}

func (c ProwlarrCredentials) HasCredentials() bool { return acquisition.ValidProwlarrAPIKey(c.APIKey) }
func (ProwlarrCredentials) String() string         { return "ProwlarrCredentials{key:<redacted>}" }
func (c ProwlarrCredentials) GoString() string     { return c.String() }

type ProwlarrReferenceDirectory string

func (ProwlarrReferenceDirectory) String() string     { return "<private-prowlarr-directory>" }
func (d ProwlarrReferenceDirectory) GoString() string { return d.String() }

type prowlarrSettingsDocument struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url,omitempty"`
}

func ValidateProwlarrSettings(settings ProwlarrSettings) error {
	if settings.BaseURL == "" {
		if settings.Enabled {
			return ErrInvalidRuntimeSettings
		}
		return nil
	}
	if _, err := acquisition.NormalizeProwlarrBaseURL(settings.BaseURL); err != nil {
		return ErrInvalidRuntimeSettings
	}
	return nil
}

func normalizeProwlarrSettings(settings ProwlarrSettings) ProwlarrSettings {
	if settings.BaseURL != "" {
		settings.BaseURL, _ = acquisition.NormalizeProwlarrBaseURL(settings.BaseURL)
	}
	return settings
}

func nonEmptyProwlarrSettings(s ProwlarrSettings) *prowlarrSettingsDocument {
	if s == (ProwlarrSettings{}) {
		return nil
	}
	s = normalizeProwlarrSettings(s)
	return &prowlarrSettingsDocument{Enabled: s.Enabled, BaseURL: s.BaseURL}
}

func (store *FileProviderConfigStore) SaveProwlarr(credentials ProwlarrCredentials) error {
	if !credentials.HasCredentials() {
		return ErrProviderCredentialsIncomplete
	}
	return store.update(func(c *ProviderConfig) { c.Prowlarr = credentials })
}

// Clear is explicit, never inferred from an incomplete Save bundle.
func (store *FileProviderConfigStore) ClearProwlarr() error {
	return store.update(func(c *ProviderConfig) { c.Prowlarr = ProwlarrCredentials{} })
}

func decodeProwlarrCredentials(raw []byte) (ProwlarrCredentials, bool) {
	fields, ok := decodeProviderFields(raw)
	if !ok {
		return ProwlarrCredentials{}, false
	}
	key, ok := decodeRequiredString(fields, "api_key")
	if !ok {
		return ProwlarrCredentials{}, false
	}
	c := ProwlarrCredentials{APIKey: key}
	return c, c.HasCredentials()
}

func loadProwlarrSettings(settings ProwlarrSettings) (ProwlarrSettings, ProwlarrCredentials, ProwlarrReferenceDirectory, error) {
	if ValidateProwlarrSettings(settings) != nil {
		return ProwlarrSettings{}, ProwlarrCredentials{}, "", ErrInvalidRuntimeSettings
	}
	settings = normalizeProwlarrSettings(settings)
	store, err := NewDefaultFileProviderConfigStore()
	if err != nil {
		return ProwlarrSettings{}, ProwlarrCredentials{}, "", ErrProviderConfigUnavailable
	}
	directory := ProwlarrReferenceDirectory(filepath.Join(filepath.Dir(store.path), "acquisition", "prowlarr"))
	// Disabled means no credentials are consumed and no directories opened.
	if !settings.Enabled {
		return settings, ProwlarrCredentials{}, directory, nil
	}
	providers, err := store.Load()
	if err != nil {
		return ProwlarrSettings{}, ProwlarrCredentials{}, "", ErrProviderConfigUnavailable
	}
	return settings, providers.Prowlarr, directory, nil
}
