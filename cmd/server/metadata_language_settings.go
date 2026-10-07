package main

import (
	"sync"

	"lernae/internal/config"
	"lernae/internal/providers/openlibrary"
)

type runtimeMetadataLanguageSettings struct {
	mu       sync.Mutex
	store    *config.FileRuntimeSettingsStore
	provider *openlibrary.SearchSource
}

func (settings *runtimeMetadataLanguageSettings) GetMetadataLanguage() (config.MetadataLanguage, error) {
	if settings == nil || settings.store == nil {
		return "", config.ErrRuntimeSettingsPath
	}
	runtime, err := settings.store.Load()
	if err != nil {
		return "", err
	}
	language := config.EffectiveMetadataLanguage(runtime.Metadata.Language)
	if !language.Valid() {
		return "", config.ErrInvalidRuntimeSettings
	}
	return language, nil
}

func (settings *runtimeMetadataLanguageSettings) SetMetadataLanguage(language config.MetadataLanguage) error {
	if settings == nil || settings.store == nil {
		return config.ErrRuntimeSettingsPath
	}
	if !language.Valid() {
		return config.ErrInvalidRuntimeSettings
	}
	settings.mu.Lock()
	defer settings.mu.Unlock()
	if err := settings.store.Update(func(runtime *config.RuntimeSettings) {
		runtime.Metadata.Language = language
	}); err != nil {
		return err
	}
	if settings.provider != nil {
		settings.provider.SetMetadataLanguage(string(language))
	}
	return nil
}
