package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	rommMaxResponseBytes = 8 << 20
	rommPageSize         = 100
	rommMaxPages         = 100
	rommMaxRecords       = 10_000
	rommDefaultTimeout   = 10 * time.Second
	rommMaxTimeout       = time.Minute
	rommGameCubeSlug     = "ngc"
)

var (
	ErrRomMInvalidConfiguration = errors.New("RomM inventory configuration is invalid")
	ErrRomMAuthentication       = errors.New("RomM inventory authentication failed")
	ErrRomMProviderUnavailable  = errors.New("RomM inventory provider is unavailable")
	ErrRomMInvalidResponse      = errors.New("RomM inventory response is invalid")
	ErrRomMResponseTooLarge     = errors.New("RomM inventory response exceeds the safety limit")
	ErrRomMAmbiguousInventory   = errors.New("RomM inventory contains ambiguous matching records")
)

// RomMInventorySourceOptions configures a read-only RomM inventory client.
// The token and base URL are supplied by trusted local configuration; they are
// never derived from inventory requests.
type RomMInventorySourceOptions struct {
	BaseURL        string        `json:"-"`
	Token          string        `json:"-"`
	HTTPClient     *http.Client  `json:"-"`
	RequestTimeout time.Duration `json:"-"`
}

func (options RomMInventorySourceOptions) String() string {
	configured := strings.TrimSpace(options.BaseURL) != "" && strings.TrimSpace(options.Token) != ""
	return "RomMInventorySourceOptions{configured:" + strconv.FormatBool(configured) + "}"
}

func (options RomMInventorySourceOptions) GoString() string {
	return options.String()
}

// RomMInventorySource implements the bounded GameCube inventory slice using
// RomM's read-only API. It intentionally does not implement StorageSource:
// RomM file paths are not trusted restore locators.
type RomMInventorySource struct {
	baseURL        url.URL
	token          string
	client         *http.Client
	requestTimeout time.Duration
}

var _ providers.InventorySource = (*RomMInventorySource)(nil)

func (source *RomMInventorySource) String() string {
	if source == nil {
		return "RomMInventorySource{configured:false}"
	}
	return "RomMInventorySource{configured:" + strconv.FormatBool(source.token != "") + "}"
}

func (source *RomMInventorySource) GoString() string {
	return source.String()
}

func NewRomMInventorySource(options RomMInventorySourceOptions) (*RomMInventorySource, error) {
	baseURL, err := parseRomMBaseURL(options.BaseURL)
	if err != nil || !validRomMToken(options.Token) {
		return nil, ErrRomMInvalidConfiguration
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = rommDefaultTimeout
	}
	if options.RequestTimeout > rommMaxTimeout {
		return nil, ErrRomMInvalidConfiguration
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &RomMInventorySource{
		baseURL:        *baseURL,
		token:          options.Token,
		client:         client,
		requestTimeout: options.RequestTimeout,
	}, nil
}

func parseRomMBaseURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || trimmed == "" || !parsed.IsAbs() || parsed.Host == "" || parsed.Opaque != "" {
		return nil, ErrRomMInvalidConfiguration
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, ErrRomMInvalidConfiguration
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.RawPath != "" {
		return nil, ErrRomMInvalidConfiguration
	}
	if strings.ContainsAny(parsed.Path, "\\\x00") || containsControl(parsed.Path) {
		return nil, ErrRomMInvalidConfiguration
	}
	pathWithoutTrailingSlash := strings.TrimRight(parsed.Path, "/")
	if pathWithoutTrailingSlash != "" && path.Clean(pathWithoutTrailingSlash) != pathWithoutTrailingSlash {
		return nil, ErrRomMInvalidConfiguration
	}
	parsed.Path = pathWithoutTrailingSlash
	return parsed, nil
}

func validRomMToken(token string) bool {
	if token == "" {
		return false
	}
	for _, value := range token {
		if value < 0x21 || value > 0x7e {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (source *RomMInventorySource) AssetsForEdition(ctx context.Context, editionID domain.EditionID) ([]domain.Asset, error) {
	if editionID != EditionKey("gamecube", "disc_image") {
		return []domain.Asset{}, nil
	}
	roms, err := source.listGameCubeRoms(ctx)
	if err != nil {
		return nil, err
	}
	assets := make([]domain.Asset, 0, len(roms))
	for _, rom := range roms {
		if !rom.hasIGDBIdentity() {
			continue
		}
		match, supported := rom.inventoryMatch()
		if !supported {
			continue
		}
		assets = append(assets, domain.Asset{
			ID:             match.AssetID,
			EditionID:      EditionKey("gamecube", "disc_image"),
			Kind:           "disc_image",
			TotalSizeBytes: match.TotalSizeBytes,
		})
	}
	return assets, nil
}

func (source *RomMInventorySource) FindMatchingAssets(ctx context.Context, query providers.InventoryQuery) ([]providers.InventoryMatch, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if query.WorkIdentity.Provider != "igdb" || query.Platform != "gamecube" || query.Format != "disc_image" {
		return []providers.InventoryMatch{}, nil
	}
	externalID, ok := canonicalIGDBID(query.WorkIdentity.ExternalID)
	if !ok {
		return []providers.InventoryMatch{}, nil
	}
	roms, err := source.listGameCubeRoms(ctx)
	if err != nil {
		return nil, err
	}
	var matching *rommInventoryRecord
	for index := range roms {
		rom := &roms[index]
		if rom.IGDBID == nil || *rom.IGDBID != externalID {
			continue
		}
		if matching != nil {
			return nil, ErrRomMAmbiguousInventory
		}
		matching = rom
	}
	if matching == nil {
		return []providers.InventoryMatch{}, nil
	}
	match, supported := matching.inventoryMatch()
	if !supported {
		return []providers.InventoryMatch{}, nil
	}
	return []providers.InventoryMatch{match}, nil
}

func canonicalIGDBID(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
		return 0, false
	}
	return parsed, true
}

func (source *RomMInventorySource) listGameCubeRoms(parent context.Context) ([]rommInventoryRecord, error) {
	ctx, cancel := context.WithTimeout(parent, source.requestTimeout)
	defer cancel()

	var platforms []rommPlatform
	if err := source.getJSON(ctx, "/api/platforms", nil, &platforms, '['); err != nil {
		return nil, err
	}
	if platforms == nil {
		return nil, ErrRomMInvalidResponse
	}
	var platformID int64
	for _, platform := range platforms {
		if platform.ID == nil || platform.Slug == nil {
			return nil, ErrRomMInvalidResponse
		}
		if *platform.Slug != rommGameCubeSlug {
			continue
		}
		if platformID != 0 {
			return nil, ErrRomMInvalidResponse
		}
		if *platform.ID <= 0 {
			return nil, ErrRomMInvalidResponse
		}
		platformID = *platform.ID
	}
	if platformID == 0 {
		return []rommInventoryRecord{}, nil
	}

	var all []rommInventoryRecord
	var expectedTotal int64 = -1
	seenIDs := make(map[int64]struct{})
	for pageNumber := 0; pageNumber < rommMaxPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offset := int64(pageNumber * rommPageSize)
		query := url.Values{
			"platform_ids":       {strconv.FormatInt(platformID, 10)},
			"metadata_providers": {"igdb"},
			"group_by_meta_id":   {"false"},
			"with_files":         {"false"},
			"with_char_index":    {"false"},
			"with_filter_values": {"false"},
			"limit":              {strconv.Itoa(rommPageSize)},
			"offset":             {strconv.FormatInt(offset, 10)},
		}
		var page rommInventoryPage
		if err := source.getJSON(ctx, "/api/roms", query, &page, '{'); err != nil {
			return nil, err
		}
		if page.Items == nil || page.Total == nil || page.Limit == nil || page.Offset == nil {
			return nil, ErrRomMInvalidResponse
		}
		if *page.Total < 0 || *page.Total > rommMaxRecords || *page.Limit != rommPageSize || *page.Offset != offset {
			return nil, ErrRomMInvalidResponse
		}
		if expectedTotal == -1 {
			expectedTotal = *page.Total
		} else if *page.Total != expectedTotal {
			return nil, ErrRomMInvalidResponse
		}
		if offset > expectedTotal {
			return nil, ErrRomMInvalidResponse
		}
		expectedItems := expectedTotal - offset
		if expectedItems > rommPageSize {
			expectedItems = rommPageSize
		}
		if int64(len(page.Items)) != expectedItems {
			return nil, ErrRomMInvalidResponse
		}
		if len(all)+len(page.Items) > rommMaxRecords {
			return nil, ErrRomMInvalidResponse
		}
		for _, rom := range page.Items {
			if rom.ID == nil || *rom.ID <= 0 || rom.PlatformID == nil || rom.PlatformSlug == nil {
				return nil, ErrRomMInvalidResponse
			}
			if *rom.PlatformID != platformID || *rom.PlatformSlug != rommGameCubeSlug {
				return nil, ErrRomMInvalidResponse
			}
			if _, exists := seenIDs[*rom.ID]; exists {
				return nil, ErrRomMInvalidResponse
			}
			seenIDs[*rom.ID] = struct{}{}
			all = append(all, rom)
		}
		if int64(len(all)) == expectedTotal {
			return all, nil
		}
	}
	return nil, ErrRomMInvalidResponse
}

func (source *RomMInventorySource) getJSON(ctx context.Context, endpoint string, query url.Values, target any, expectedFirstByte byte) error {
	requestURL := source.baseURL
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + endpoint
	requestURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return ErrRomMInvalidConfiguration
	}
	request.Header.Set("Authorization", "Bearer "+source.token)
	request.Header.Set("Accept", "application/json")
	response, err := source.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrRomMProviderUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrRomMAuthentication
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ErrRomMProviderUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, rommMaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrRomMProviderUnavailable
	}
	if len(body) > rommMaxResponseBytes {
		return ErrRomMResponseTooLarge
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != expectedFirstByte {
		return ErrRomMInvalidResponse
	}
	if err := json.Unmarshal(trimmed, target); err != nil {
		return ErrRomMInvalidResponse
	}
	return nil
}

type rommPlatform struct {
	ID   *int64  `json:"id"`
	Slug *string `json:"slug"`
}

type rommInventoryPage struct {
	Items  []rommInventoryRecord `json:"items"`
	Total  *int64                `json:"total"`
	Limit  *int64                `json:"limit"`
	Offset *int64                `json:"offset"`
}

// This deliberately decodes only inventory evidence needed by the Phase 1
// adapter. RomM's fs_path, full_path, and file-list values are ignored.
type rommInventoryRecord struct {
	ID                  *int64  `json:"id"`
	IGDBID              *int64  `json:"igdb_id"`
	PlatformID          *int64  `json:"platform_id"`
	PlatformSlug        *string `json:"platform_slug"`
	FSExtension         *string `json:"fs_extension"`
	FSSizeBytes         *int64  `json:"fs_size_bytes"`
	HasSimpleSingleFile *bool   `json:"has_simple_single_file"`
	HasNestedSingleFile *bool   `json:"has_nested_single_file"`
	HasMultipleFiles    *bool   `json:"has_multiple_files"`
}

func (rom rommInventoryRecord) hasIGDBIdentity() bool {
	return rom.IGDBID != nil && *rom.IGDBID > 0
}

func (rom rommInventoryRecord) inventoryMatch() (providers.InventoryMatch, bool) {
	if rom.ID == nil || *rom.ID <= 0 || !rom.hasIGDBIdentity() || rom.FSExtension == nil || rom.FSSizeBytes == nil || *rom.FSSizeBytes <= 0 {
		return providers.InventoryMatch{}, false
	}
	if rom.HasSimpleSingleFile == nil || !*rom.HasSimpleSingleFile || rom.HasNestedSingleFile == nil || *rom.HasNestedSingleFile || rom.HasMultipleFiles == nil || *rom.HasMultipleFiles {
		return providers.InventoryMatch{}, false
	}
	extension := strings.ToLower(strings.TrimSpace(*rom.FSExtension))
	extension = strings.TrimPrefix(extension, ".")
	if extension != "iso" {
		return providers.InventoryMatch{}, false
	}
	filename := fmt.Sprintf("romm-%d.iso", *rom.ID)
	return providers.InventoryMatch{
		AssetID:        domain.AssetID(fmt.Sprintf("romm-%d", *rom.ID)),
		Platform:       "gamecube",
		Format:         "disc_image",
		TotalSizeBytes: *rom.FSSizeBytes,
		Parts: []providers.InventoryPart{{
			Role: "rom", Filename: filename, SizeBytes: *rom.FSSizeBytes,
		}},
	}, true
}
