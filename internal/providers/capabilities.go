// Package providers defines independent integration capabilities. These are
// contracts only; Phase 0 does not connect to external providers.
package providers

import (
	"context"

	"lernae/internal/domain"
)

type MetadataSource interface {
	SearchWorks(context.Context, string) ([]domain.Work, error)
}

type InventorySource interface {
	AssetsForEdition(context.Context, domain.EditionID) ([]domain.Asset, error)
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
