// Package providers defines independent, provider-neutral integration
// capabilities shared by the Server.
package providers

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"lernae/internal/domain"
)

const (
	MaxUniversalSearchQueryBytes = 200
	MaxProviderSearchResults     = 20
	MaxSearchResponseBytes       = 1 << 20
)

type MetadataSource interface {
	SearchWorks(context.Context, string) ([]domain.MetadataSearchResult, error)
}

// SearchProvider returns one provider's relevance-ranked results. Implementors
// must honor context cancellation and the requested result limit.
type SearchProvider interface {
	Name() string
	Search(context.Context, string, int) ([]domain.MetadataSearchResult, error)
}

// UniversalSearchSource aggregates provider-local results into the public
// response envelope.
type UniversalSearchSource interface {
	Search(context.Context, string, int) (domain.SearchResponse, error)
}

var (
	ErrSearchThrottled            = errors.New("search provider throttled")
	ErrSearchUnavailable          = errors.New("search provider unavailable")
	ErrSearchInvalid              = errors.New("invalid search query")
	ErrSearchMalformed            = errors.New("search provider returned an invalid response")
	ErrSearchBodyTooLarge         = errors.New("search provider response exceeded its size limit")
	ErrSearchStale                = errors.New("stale cached search results")
	ErrDetailsUnsupportedProvider = errors.New("work details provider is unsupported")
	ErrDetailsInvalidExternalID   = errors.New("work details external ID is invalid")
	ErrDetailsWorkNotFound        = errors.New("work details were not found")
	ErrDetailsProviderUnavailable = errors.New("work details provider is unavailable")
	ErrDetailsMalformedResponse   = errors.New("work details provider returned an invalid response")
	ErrDetailsResponseTooLarge    = errors.New("work details response exceeded its size limit")
	ErrDetailsRateLimited         = errors.New("work details provider rate limit was reached")
)

// ValidateSearchRequest applies the shared provider input and result bounds.
func ValidateSearchRequest(query string, limit int) error {
	if !utf8.ValidString(query) || len(query) > MaxUniversalSearchQueryBytes || limit < 1 || limit > MaxProviderSearchResults {
		return ErrSearchInvalid
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return ErrSearchInvalid
	}
	for _, character := range query {
		if unicode.IsControl(character) || unicode.In(character, unicode.Zl, unicode.Zp) {
			return ErrSearchInvalid
		}
	}
	return nil
}

// WorkDetailsSource loads provider-neutral canonical metadata for one external
// Work identity. Implementations may dispatch by provider when they aggregate
// more than one concrete metadata adapter.
type WorkDetailsSource interface {
	GetWorkDetails(context.Context, string, string) (domain.WorkDetails, error)
}

type InventorySource interface {
	AssetsForEdition(context.Context, domain.EditionID) ([]domain.Asset, error)
	FindMatchingAssets(context.Context, InventoryQuery) ([]InventoryMatch, error)
}

// ExternalWorkIdentity identifies a work in a provider-neutral catalog.
type ExternalWorkIdentity struct {
	Provider   string
	ExternalID string
}

// InventoryQuery describes the exact work and edition intent to resolve
// against an inventory source. Platform and Format are concrete attributes,
// not canonical Edition IDs.
type InventoryQuery struct {
	WorkIdentity ExternalWorkIdentity
	Platform     string
	Format       string
}

// InventoryMatch is the provider-neutral summary and physical-part list of an
// inventory asset that satisfies an InventoryQuery.
type InventoryMatch struct {
	AssetID        domain.AssetID
	WorkTitle      string
	Platform       string
	Format         string
	TotalSizeBytes int64
	Parts          []InventoryPart
}

// InventoryPart contains provider-neutral details needed to persist an
// inventory asset's physical members. The catalog assigns canonical IDs.
type InventoryPart struct {
	PartIndex    *int
	Role         string
	Filename     string
	RelativePath string
	SizeBytes    int64
}

// StorageSource reports locations only. Physical transfers belong to the
// Agent and are not implemented by this contract package.
type StorageSource interface {
	LocationsForAsset(context.Context, domain.AssetID) ([]domain.AssetLocation, error)
}

type AcquisitionSource interface {
	RequestEdition(context.Context, domain.EditionID) (AcquisitionRequest, error)
	GetRequestStatus(context.Context, AcquisitionRequest) (AcquisitionStatus, error)
}

type AcquisitionRequest struct {
	ProviderRequestID string
}

type AcquisitionStatus struct {
	State  string
	Assets []domain.Asset
}
