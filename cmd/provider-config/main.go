package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"lernae/internal/acquisition"
	"lernae/internal/config"
	"lernae/internal/providers/wikidata"
)

var (
	errUsage                         = errors.New("usage: provider-config configure | configure [igdb|romm|local|prowlarr|qbittorrent] | status | dev-api-proxy-target")
	errProviderConfig                = errors.New("provider config is unavailable or unsafe; repair the local provider config and retry")
	errProviderConfigSave            = errors.New("provider config could not be saved safely")
	errRuntimeSettings               = errors.New("runtime settings are unavailable or unsafe; repair the local settings and retry")
	errRuntimeSettingsSave           = errors.New("runtime settings could not be saved safely")
	errRomMInventoryNeedsCredentials = errors.New("RomM inventory requires saved credentials; configure them with make configure-romm")
	errInvalidWikidataContact        = errors.New("Wikidata contact email is invalid")
	errPasswordInput                 = errors.New("credentials must be entered through a terminal")
	errSettingsInput                 = errors.New("runtime settings must be entered through a terminal")
	errCommandOutput                 = errors.New("provider config output failed")
)

type secretReader interface {
	ReadSecret(prompt string) (string, error)
}

type lineReader interface {
	ReadLine(prompt string) (string, error)
}

type runtimeSettingsStore interface {
	Load() (config.RuntimeSettings, error)
	Update(func(*config.RuntimeSettings)) error
}

// sanitizedCauseError preserves error classification without exposing details
// such as schema diagnostics or paths through the operator-facing message.
type sanitizedCauseError struct {
	public error
	cause  error
}

func (err sanitizedCauseError) Error() string {
	return err.public.Error()
}

func (err sanitizedCauseError) Unwrap() []error {
	return []error{err.public, err.cause}
}

func withSanitizedCause(public, cause error) error {
	if cause == nil {
		return public
	}
	return sanitizedCauseError{public: public, cause: cause}
}

type operation int

const (
	operationStatus operation = iota
	operationConfigureIGDB
	operationConfigureRomM
	operationConfigureAll
	operationDevAPIProxyTarget
	operationConfigureLocal
	operationConfigureProwlarr
	operationConfigureQBittorrent
)

func main() {
	store, err := config.NewDefaultFileProviderConfigStore()
	if err != nil {
		fail(errProviderConfig)
		return
	}
	settings, err := config.NewDefaultFileRuntimeSettingsStore()
	if err != nil {
		fail(errRuntimeSettings)
		return
	}
	reader := hiddenTerminalReader{
		input:        os.Stdin,
		promptOutput: os.Stderr,
		readPassword: term.ReadPassword,
	}
	if err := runWithStores(os.Args[1:], store, settings, reader, terminalLineReader{input: os.Stdin, output: os.Stdout}, os.Stdout); err != nil {
		fail(err)
	}
}

func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func run(args []string, store config.ProviderConfigStore, reader secretReader, output io.Writer) error {
	return runWithStores(args, store, nil, reader, nil, output)
}

func runWithStores(args []string, store config.ProviderConfigStore, settingsStore runtimeSettingsStore, secrets secretReader, lines lineReader, output io.Writer) error {
	selected, err := parseOperation(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errCommandOutput
	}
	if selected == operationDevAPIProxyTarget {
		target, err := config.ResolvedServerAPIProxyTarget()
		if err != nil {
			return errRuntimeSettings
		}
		if _, err := fmt.Fprintln(output, target); err != nil {
			return errCommandOutput
		}
		return nil
	}
	if store == nil {
		return errProviderConfig
	}

	if selected == operationStatus {
		providerConfig, err := store.Load()
		if err != nil {
			return errProviderConfig
		}
		if _, err := fmt.Fprintf(output, "igdb_configured=%t\nromm_configured=%t\n", providerConfig.IGDB.HasCredentials(), providerConfig.RomM.HasCredentials()); err != nil {
			return errCommandOutput
		}
		if settingsStore != nil {
			settings, err := settingsStore.Load()
			if err != nil {
				return withSanitizedCause(errRuntimeSettings, err)
			}
			if err := writeRuntimeSettingsStatus(output, settings); err != nil {
				return errCommandOutput
			}
			if _, err := fmt.Fprintf(output, "qbittorrent_password_present=%t\nqbittorrent_configured=%t\n", providerConfig.QBittorrent.HasCredentials(), settings.QBittorrent.BaseURL != "" && settings.QBittorrent.Username != "" && providerConfig.QBittorrent.HasCredentials()); err != nil {
				return errCommandOutput
			}
			if _, err := fmt.Fprintf(output, "prowlarr_key_present=%t\nprowlarr_configured=%t\n", providerConfig.Prowlarr.HasCredentials(), settings.Prowlarr.BaseURL != "" && providerConfig.Prowlarr.HasCredentials()); err != nil {
				return errCommandOutput
			}
		}
		return nil
	}
	if selected == operationConfigureQBittorrent {
		if settingsStore == nil || lines == nil || secrets == nil {
			return errSettingsInput
		}
		return configureQBittorrent(store, settingsStore, secrets, lines, output)
	}
	if selected == operationConfigureProwlarr {
		if settingsStore == nil || lines == nil || secrets == nil {
			return errSettingsInput
		}
		return configureProwlarr(store, settingsStore, secrets, lines, output)
	}
	if selected == operationConfigureLocal {
		if settingsStore == nil || lines == nil {
			return errSettingsInput
		}
		return configureLocal(settingsStore, lines, output)
	}
	if selected == operationConfigureAll {
		if settingsStore == nil || lines == nil {
			return errSettingsInput
		}
		return configureAll(store, settingsStore, secrets, lines, output)
	}
	if secrets == nil {
		return errPasswordInput
	}

	firstPrompt, secondPrompt := "", ""
	switch selected {
	case operationConfigureIGDB:
		firstPrompt, secondPrompt = "IGDB client ID: ", "IGDB client secret: "
	case operationConfigureRomM:
		firstPrompt, secondPrompt = "RomM base URL: ", "RomM client API token: "
	default:
		return errUsage
	}

	first, err := secrets.ReadSecret(firstPrompt)
	if err != nil {
		return errPasswordInput
	}
	second, err := secrets.ReadSecret(secondPrompt)
	if err != nil {
		return errPasswordInput
	}

	switch selected {
	case operationConfigureIGDB:
		err = store.SaveIGDB(config.IGDBCredentials{ClientID: first, ClientSecret: second})
	case operationConfigureRomM:
		err = store.SaveRomM(config.RomMCredentials{BaseURL: first, ClientAPIToken: second})
	}
	if err != nil {
		return errProviderConfigSave
	}
	if _, err := io.WriteString(output, "Provider configuration saved.\n"); err != nil {
		return errCommandOutput
	}
	return nil
}

func configureAll(providerStore config.ProviderConfigStore, settingsStore runtimeSettingsStore, secrets secretReader, lines lineReader, output io.Writer) error {
	current, err := settingsStore.Load()
	if err != nil {
		return withSanitizedCause(errRuntimeSettings, err)
	}
	providerConfig, err := providerStore.Load()
	if err != nil {
		return errProviderConfig
	}
	if err := writeRuntimeSettingsStatus(output, current); err != nil {
		return errCommandOutput
	}

	fields := []struct {
		name  string
		value string
		set   func(*config.RuntimeSettings, string)
	}{
		{"Server database path", current.Server.DatabasePath, func(s *config.RuntimeSettings, v string) { s.Server.DatabasePath = v }},
		{"Server listen address", current.Server.ListenAddress, func(s *config.RuntimeSettings, v string) { s.Server.ListenAddress = v }},
		{"Agent socket path", current.Agent.SocketPath, func(s *config.RuntimeSettings, v string) { s.Agent.SocketPath = v }},
		{"Agent cache path", current.Agent.CachePath, func(s *config.RuntimeSettings, v string) { s.Agent.CachePath = v }},
		{"Agent staging path", current.Agent.StagingPath, func(s *config.RuntimeSettings, v string) { s.Agent.StagingPath = v }},
		{"Inventory source (manifest or romm)", current.Inventory.Source, func(s *config.RuntimeSettings, v string) { s.Inventory.Source = v }},
		{"Inventory Manifest path", current.Inventory.ManifestPath, func(s *config.RuntimeSettings, v string) { s.Inventory.ManifestPath = v }},
		{"Wikidata contact email", current.Wikidata.ContactEmail, func(s *config.RuntimeSettings, v string) { s.Wikidata.ContactEmail = v }},
	}
	proposed := current
	updates := make([]func(*config.RuntimeSettings), 0, len(fields))
	for _, field := range fields {
		prompt := field.name + " (Enter keeps saved/default; 'default' clears): "
		if field.name == "Wikidata contact email" {
			if wikidataContactConfigured(current.Wikidata.ContactEmail) {
				prompt = "Wikidata contact email (configured; Enter keeps saved value, 'default' clears): "
			} else {
				prompt = "Wikidata contact email (not configured; Enter leaves it unset, 'default' clears): "
			}
		}
		value, readErr := lines.ReadLine(prompt)
		if readErr != nil {
			return errSettingsInput
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.EqualFold(value, "default") {
			value = ""
		}
		if field.name == "Wikidata contact email" && value != "" {
			if _, err := wikidata.NewClient(wikidata.ClientConfig{ContactEmail: value}); err != nil {
				return errInvalidWikidataContact
			}
		}
		field.set(&proposed, value)
		setValue := value
		setField := field.set
		updates = append(updates, func(settings *config.RuntimeSettings) { setField(settings, setValue) })
	}
	if len(updates) > 0 {
		if err := config.ValidateRuntimeSettings(proposed); err != nil {
			return errRuntimeSettingsSave
		}
	}

	for _, provider := range []struct {
		name       string
		configured bool
		configure  func() error
	}{
		{"IGDB", providerConfig.IGDB.HasCredentials(), func() error {
			clientID, readErr := secrets.ReadSecret("IGDB client ID: ")
			if readErr != nil {
				return errPasswordInput
			}
			clientSecret, readErr := secrets.ReadSecret("IGDB client secret: ")
			if readErr != nil {
				return errPasswordInput
			}
			return providerStore.SaveIGDB(config.IGDBCredentials{ClientID: clientID, ClientSecret: clientSecret})
		}},
		{"RomM", providerConfig.RomM.HasCredentials(), func() error {
			baseURL, readErr := secrets.ReadSecret("RomM base URL: ")
			if readErr != nil {
				return errPasswordInput
			}
			token, readErr := secrets.ReadSecret("RomM client API token: ")
			if readErr != nil {
				return errPasswordInput
			}
			return providerStore.SaveRomM(config.RomMCredentials{BaseURL: baseURL, ClientAPIToken: token})
		}},
	} {
		answer, readErr := lines.ReadLine(fmt.Sprintf("Configure %s credentials (currently configured=%t)? [y/N]: ", provider.name, provider.configured))
		if readErr != nil {
			return errSettingsInput
		}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			continue
		}
		if secrets == nil {
			return errPasswordInput
		}
		if err := provider.configure(); err != nil {
			return errProviderConfigSave
		}
	}
	if proposed.Inventory.Source == "romm" {
		persistedProviders, err := providerStore.Load()
		if err != nil {
			return errProviderConfig
		}
		if !persistedProviders.RomM.HasCredentials() {
			return errRomMInventoryNeedsCredentials
		}
	}
	if len(updates) > 0 {
		var validationErr error
		if err := settingsStore.Update(func(saved *config.RuntimeSettings) {
			candidate := *saved
			for _, update := range updates {
				update(&candidate)
			}
			if err := config.ValidateRuntimeSettings(candidate); err != nil {
				validationErr = err
				return
			}
			*saved = candidate
		}); err != nil {
			return withSanitizedCause(errRuntimeSettingsSave, err)
		}
		if validationErr != nil {
			return withSanitizedCause(errRuntimeSettingsSave, validationErr)
		}
	}
	if _, err := io.WriteString(output, "Configuration saved.\n"); err != nil {
		return errCommandOutput
	}
	return nil
}

// configureQBittorrent disables before password replacement so a failed
// two-store update cannot enable a half-configured client. No HTTP is used.
func configureQBittorrent(providers config.ProviderConfigStore, store runtimeSettingsStore, secrets secretReader, lines lineReader, output io.Writer) error {
	passwordStore, ok := providers.(interface {
		SaveQBittorrent(config.QBittorrentCredentials) error
		ClearQBittorrent() error
	})
	if !ok {
		return errProviderConfigSave
	}
	current, err := store.Load()
	if err != nil {
		return errRuntimeSettings
	}
	credentials, err := providers.Load()
	if err != nil {
		return errProviderConfig
	}
	enabled, err := lines.ReadLine("Enable qBittorrent execution (true/false; Enter keeps saved; default disables): ")
	if err != nil {
		return errSettingsInput
	}
	enabled = strings.TrimSpace(enabled)
	if enabled != "" && enabled != "true" && enabled != "false" && enabled != "default" {
		return errRuntimeSettingsSave
	}
	endpoint, err := lines.ReadLine("qBittorrent HTTP(S) base URL (Enter keeps saved; default clears and disables): ")
	if err != nil {
		return errSettingsInput
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "" && endpoint != "default" {
		endpoint, err = acquisition.NormalizeQBittorrentBaseURL(endpoint)
		if err != nil {
			return errRuntimeSettingsSave
		}
	}
	username, err := lines.ReadLine("qBittorrent username (Enter keeps saved; default clears and disables): ")
	if err != nil {
		return errSettingsInput
	}
	if username != "" && username != "default" && !acquisition.ValidQBittorrentCredential(username) {
		return errRuntimeSettingsSave
	}
	password, err := secrets.ReadSecret("qBittorrent password (Enter keeps saved; default clears and disables): ")
	if err != nil {
		return errPasswordInput
	}
	if password != "" && password != "default" && !acquisition.ValidQBittorrentCredential(password) {
		return errProviderConfigSave
	}
	apply := func(s *config.RuntimeSettings) {
		if enabled != "" {
			s.QBittorrent.Enabled = enabled == "true"
		}
		if endpoint == "default" {
			s.QBittorrent.BaseURL, s.QBittorrent.Enabled = "", false
		} else if endpoint != "" {
			s.QBittorrent.BaseURL = endpoint
		}
		if username == "default" {
			s.QBittorrent.Username, s.QBittorrent.Enabled = "", false
		} else if username != "" {
			s.QBittorrent.Username = username
		}
		if password == "default" {
			s.QBittorrent.Enabled = false
		}
	}
	proposed := current
	apply(&proposed)
	hasPassword := credentials.QBittorrent.HasCredentials()
	if password != "" {
		hasPassword = password != "default"
	}
	if config.ValidateQBittorrentSettings(proposed.QBittorrent) != nil || (proposed.QBittorrent.Enabled && !hasPassword) {
		return errRuntimeSettingsSave
	}
	if password != "" {
		if err := store.Update(func(s *config.RuntimeSettings) { s.QBittorrent.Enabled = false }); err != nil {
			return errRuntimeSettingsSave
		}
		if password == "default" {
			err = passwordStore.ClearQBittorrent()
		} else {
			err = passwordStore.SaveQBittorrent(config.QBittorrentCredentials{Password: password})
		}
		if err != nil {
			return errProviderConfigSave
		}
	}
	var validationErr error
	err = store.Update(func(saved *config.RuntimeSettings) {
		candidate := *saved
		apply(&candidate)
		if password != "" && password != "default" && enabled == "" && endpoint != "default" && username != "default" {
			candidate.QBittorrent.Enabled = current.QBittorrent.Enabled
		}
		if config.ValidateQBittorrentSettings(candidate.QBittorrent) != nil {
			validationErr = errRuntimeSettingsSave
			return
		}
		*saved = candidate
	})
	if err != nil || validationErr != nil {
		return errRuntimeSettingsSave
	}
	if _, err := io.WriteString(output, "qBittorrent execution configuration saved; restart the Server to apply.\n"); err != nil {
		return errCommandOutput
	}
	return nil
}

// configureProwlarr uses separate existing atomic stores. Before any credential
// change it durably disables discovery, then saves the secret, then merges the
// endpoint/enabled settings. Failure never enables a half-configured adapter;
// it may leave an unused saved credential and require another configure run.
func configureProwlarr(providers config.ProviderConfigStore, store runtimeSettingsStore, secrets secretReader, lines lineReader, output io.Writer) error {
	current, err := store.Load()
	if err != nil {
		return errRuntimeSettings
	}
	credentials, err := providers.Load()
	if err != nil {
		return errProviderConfig
	}
	enabled, err := lines.ReadLine("Enable Prowlarr discovery (true/false; Enter keeps saved; default disables): ")
	if err != nil {
		return errSettingsInput
	}
	enabled = strings.TrimSpace(enabled)
	if enabled != "" && enabled != "true" && enabled != "false" && enabled != "default" {
		return errRuntimeSettingsSave
	}
	endpoint, err := lines.ReadLine("Prowlarr HTTP(S) base URL (Enter keeps saved; default clears and disables): ")
	if err != nil {
		return errSettingsInput
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "" && endpoint != "default" {
		endpoint, err = acquisition.NormalizeProwlarrBaseURL(endpoint)
		if err != nil {
			return errRuntimeSettingsSave
		}
	}
	key, err := secrets.ReadSecret("Prowlarr API key (Enter keeps saved; default clears and disables): ")
	if err != nil {
		return errPasswordInput
	}
	if key != "" && key != "default" && !acquisition.ValidProwlarrAPIKey(key) {
		return errProviderConfigSave
	}
	apply := func(s *config.RuntimeSettings) {
		if enabled != "" {
			s.Prowlarr.Enabled = enabled == "true"
		}
		if endpoint == "default" {
			s.Prowlarr.BaseURL = ""
			s.Prowlarr.Enabled = false
		} else if endpoint != "" {
			s.Prowlarr.BaseURL = endpoint
		}
		if key == "default" {
			s.Prowlarr.Enabled = false
		}
	}
	proposed := current
	apply(&proposed)
	hasKey := credentials.Prowlarr.HasCredentials()
	if key != "" {
		hasKey = key != "default"
	}
	if config.ValidateProwlarrSettings(proposed.Prowlarr) != nil || (proposed.Prowlarr.Enabled && !hasKey) {
		return errRuntimeSettingsSave
	}
	if key != "" {
		if err := store.Update(func(s *config.RuntimeSettings) { s.Prowlarr.Enabled = false }); err != nil {
			return errRuntimeSettingsSave
		}
		if key == "default" {
			err = providers.ClearProwlarr()
		} else {
			err = providers.SaveProwlarr(config.ProwlarrCredentials{APIKey: key})
		}
		if err != nil {
			return errProviderConfigSave
		}
	}
	var validationErr error
	err = store.Update(func(saved *config.RuntimeSettings) {
		candidate := *saved
		apply(&candidate)
		if key != "" && key != "default" && enabled == "" && endpoint != "default" {
			candidate.Prowlarr.Enabled = current.Prowlarr.Enabled
		}
		if config.ValidateProwlarrSettings(candidate.Prowlarr) != nil {
			validationErr = errRuntimeSettingsSave
			return
		}
		*saved = candidate
	})
	if err != nil || validationErr != nil {
		return errRuntimeSettingsSave
	}
	if _, err := io.WriteString(output, "Prowlarr discovery configuration saved; restart the Server to apply.\n"); err != nil {
		return errCommandOutput
	}
	return nil
}

// configureLocal merges only answered fields while the existing store owns the
// cross-process lock. Paths are never echoed in status or prompts.
func configureLocal(store runtimeSettingsStore, lines lineReader, output io.Writer) error {
	current, err := store.Load()
	if err != nil {
		return withSanitizedCause(errRuntimeSettings, err)
	}
	if err := writeRuntimeSettingsStatus(output, current); err != nil {
		return errCommandOutput
	}
	enabled, err := lines.ReadLine("Enable Server-local acquisition (true/false; Enter keeps saved; default disables): ")
	if err != nil {
		return errSettingsInput
	}
	enabled = strings.TrimSpace(enabled)
	if enabled != "" && enabled != "true" && enabled != "false" && enabled != "default" {
		return errRuntimeSettingsSave
	}
	source, err := lines.ReadLine("Local source root (existing directory; Enter keeps saved; default clears): ")
	if err != nil {
		return errSettingsInput
	}
	staging, err := lines.ReadLine("Server acquisition staging root (existing private directory; Enter keeps saved; default clears): ")
	if err != nil {
		return errSettingsInput
	}
	source, staging = strings.TrimSpace(source), strings.TrimSpace(staging)
	var validationErr error
	err = store.Update(func(saved *config.RuntimeSettings) {
		candidate := *saved
		if enabled != "" {
			candidate.LocalAcquisition.Enabled = enabled == "true"
		}
		if source != "" {
			if source == "default" {
				candidate.LocalAcquisition.SourceRoot = ""
			} else {
				candidate.LocalAcquisition.SourceRoot = source
			}
		}
		if staging != "" {
			if staging == "default" {
				candidate.LocalAcquisition.StagingRoot = ""
			} else {
				candidate.LocalAcquisition.StagingRoot = staging
			}
		}
		if err := config.ValidateRuntimeSettings(candidate); err != nil {
			validationErr = err
			return
		}
		if candidate.LocalAcquisition.Enabled {
			if err := acquisition.CheckLocalRoots(candidate.LocalAcquisition.SourceRoot, candidate.LocalAcquisition.StagingRoot); err != nil {
				validationErr = err
				return
			}
		}
		*saved = candidate
	})
	if err != nil || validationErr != nil {
		return errRuntimeSettingsSave
	}
	if _, err := io.WriteString(output, "Local acquisition configuration saved; restart the Server to apply.\n"); err != nil {
		return errCommandOutput
	}
	return nil
}

func writeRuntimeSettingsStatus(output io.Writer, settings config.RuntimeSettings) error {
	value := func(value, fallback string) string {
		if value != "" {
			return value
		}
		return fallback
	}
	_, err := fmt.Fprintf(output,
		"server_database_path=%s\nserver_listen_address=%s\nagent_socket_path=%s\nagent_cache_path=%s\nagent_staging_path=%s\ninventory_source=%s\ninventory_manifest_path=%s\nwikidata_configured=%t\n",
		value(settings.Server.DatabasePath, "(default)"),
		value(settings.Server.ListenAddress, "(default: "+config.DefaultServerListenAddress+")"),
		value(settings.Agent.SocketPath, "(default)"),
		value(settings.Agent.CachePath, "(default)"),
		value(settings.Agent.StagingPath, "(derived from cache)"),
		value(settings.Inventory.Source, config.InventorySourceManifest+" (default)"),
		value(settings.Inventory.ManifestPath, "(not configured)"),
		wikidataContactConfigured(settings.Wikidata.ContactEmail),
	)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "qbittorrent_enabled=%t\nqbittorrent_endpoint_present=%t\nqbittorrent_username_present=%t\n", settings.QBittorrent.Enabled, settings.QBittorrent.BaseURL != "", settings.QBittorrent.Username != ""); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "prowlarr_enabled=%t\nprowlarr_endpoint_present=%t\n", settings.Prowlarr.Enabled, settings.Prowlarr.BaseURL != ""); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "local_acquisition_enabled=%t\nlocal_acquisition_source_configured=%t\nlocal_acquisition_staging_configured=%t\n", settings.LocalAcquisition.Enabled, settings.LocalAcquisition.SourceRoot != "", settings.LocalAcquisition.StagingRoot != "")
	return err
}

func wikidataContactConfigured(contactEmail string) bool {
	if strings.TrimSpace(contactEmail) == "" {
		return false
	}
	_, err := wikidata.NewClient(wikidata.ClientConfig{ContactEmail: contactEmail})
	return err == nil
}

type terminalLineReader struct {
	input  io.Reader
	output io.Writer
}

func (reader terminalLineReader) ReadLine(prompt string) (string, error) {
	if reader.input == nil || reader.output == nil {
		return "", errSettingsInput
	}
	if _, err := io.WriteString(reader.output, prompt); err != nil {
		return "", errSettingsInput
	}
	var line strings.Builder
	var next [1]byte
	for {
		count, err := reader.input.Read(next[:])
		if count > 0 {
			if next[0] == '\n' {
				break
			}
			line.WriteByte(next[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && line.Len() > 0 {
				break
			}
			return "", errSettingsInput
		}
	}
	return strings.TrimRight(line.String(), "\r"), nil
}

func parseOperation(args []string) (operation, error) {
	if len(args) == 1 && args[0] == "dev-api-proxy-target" {
		return operationDevAPIProxyTarget, nil
	}
	if len(args) == 1 && args[0] == "status" {
		return operationStatus, nil
	}
	if len(args) == 1 && args[0] == "configure" {
		return operationConfigureAll, nil
	}
	if len(args) == 2 && args[0] == "configure" {
		switch args[1] {
		case "igdb":
			return operationConfigureIGDB, nil
		case "romm":
			return operationConfigureRomM, nil
		case "local":
			return operationConfigureLocal, nil
		case "prowlarr":
			return operationConfigureProwlarr, nil
		case "qbittorrent":
			return operationConfigureQBittorrent, nil
		}
	}
	return 0, errUsage
}

type hiddenTerminalReader struct {
	input        *os.File
	promptOutput io.Writer
	readPassword func(int) ([]byte, error)
}

func (reader hiddenTerminalReader) ReadSecret(prompt string) (string, error) {
	if reader.input == nil || reader.promptOutput == nil || reader.readPassword == nil {
		return "", errPasswordInput
	}
	if _, err := io.WriteString(reader.promptOutput, prompt); err != nil {
		return "", errPasswordInput
	}
	password, readErr := reader.readPassword(int(reader.input.Fd()))
	_, newlineErr := io.WriteString(reader.promptOutput, "\n")
	if readErr != nil || newlineErr != nil {
		clearPassword(password)
		return "", errPasswordInput
	}
	secret := string(password)
	clearPassword(password)
	return secret, nil
}

func clearPassword(password []byte) {
	for index := range password {
		password[index] = 0
	}
}
