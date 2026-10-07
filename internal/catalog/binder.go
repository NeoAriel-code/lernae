package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

// Availability describes what the catalog can prove about an inventory match.
type Availability string

const (
	// AvailabilityUnavailable means no exact inventory match is known.
	AvailabilityUnavailable Availability = "unavailable"
	// AvailabilityArchived means an archive location is known, but no verified local copy is proven.
	AvailabilityArchived Availability = "archived"
	// AvailabilityReady is reserved for a future verified-local-copy contract.
	AvailabilityReady Availability = "ready"
)

// BindingResult contains the canonical graph and its derived availability.
type BindingResult struct {
	Graph        WorkGraph
	Availability Availability
}

// InventoryBinder connects normalized metadata and edition intent to the
// canonical catalog graph using provider-neutral inventory capabilities.
type InventoryBinder struct {
	repository *SQLiteRepository
	inventory  providers.InventorySource
	storage    providers.StorageSource
}

// NewInventoryBinder connects catalog persistence to inventory and storage capabilities.
func NewInventoryBinder(repository *SQLiteRepository, inventorySource providers.InventorySource, storageSource providers.StorageSource) *InventoryBinder {
	return &InventoryBinder{repository: repository, inventory: inventorySource, storage: storageSource}
}

// Resolve finds an exact Work identity and Edition match, persisting or reusing
// its canonical catalog graph. Provider details are consulted only when a
// matching Asset would create a new Work binding; existing canonical Works
// remain the source of their own metadata.
func (binder *InventoryBinder) Resolve(ctx context.Context, identity providers.ExternalWorkIdentity, platform, format string, detailsSource providers.WorkDetailsSource) (BindingResult, error) {
	if err := ctx.Err(); err != nil {
		return BindingResult{}, err
	}
	if binder.repository == nil || binder.inventory == nil || binder.storage == nil {
		return BindingResult{}, errors.New("inventory binder dependencies are incomplete")
	}
	query := providers.InventoryQuery{
		WorkIdentity: identity,
		Platform:     platform,
		Format:       format,
	}
	matches, err := binder.inventory.FindMatchingAssets(ctx, query)
	if err != nil {
		return BindingResult{}, fmt.Errorf("resolve inventory match: %w", err)
	}
	if len(matches) == 0 {
		return BindingResult{Availability: AvailabilityUnavailable}, nil
	}

	existing, err := binder.repository.GetWorkByExternalIdentity(ctx, identity.Provider, identity.ExternalID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return BindingResult{}, fmt.Errorf("load existing catalog Work: %w", err)
	}
	hasExisting := err == nil
	metadata := domain.MetadataSearchResult{Provider: identity.Provider, ExternalID: identity.ExternalID}
	if !hasExisting {
		if detailsSource == nil {
			return BindingResult{}, errors.New("trusted Work details are not configured")
		}
		details, err := detailsSource.GetWorkDetails(ctx, identity.Provider, identity.ExternalID)
		if err != nil {
			return BindingResult{}, fmt.Errorf("load trusted Work details: %w", err)
		}
		if details.Provider != identity.Provider || details.ExternalID != identity.ExternalID {
			return BindingResult{}, errors.New("metadata provider returned details for a different Work identity")
		}
		if strings.TrimSpace(details.Title) == "" || !details.Medium.Valid() || strings.TrimSpace(details.WorkType) == "" {
			return BindingResult{}, errors.New("metadata provider returned incomplete Work details")
		}
		metadata.Title = details.Title
		metadata.Medium = details.Medium
		metadata.WorkType = details.WorkType
	}
	graph, err := binder.buildGraph(ctx, metadata, platform, format, matches, existing, hasExisting)
	if err != nil {
		return BindingResult{}, err
	}
	if err := binder.repository.EnsureGraph(ctx, graph); err != nil {
		return BindingResult{}, fmt.Errorf("persist canonical inventory graph: %w", err)
	}

	persisted, err := binder.repository.GetWorkByExternalIdentity(ctx, metadata.Provider, metadata.ExternalID)
	if err != nil {
		return BindingResult{}, fmt.Errorf("load persisted catalog graph: %w", err)
	}
	matchedAssets := make(map[domain.AssetID]struct{}, len(matches))
	for _, match := range matches {
		matchedAssets[match.AssetID] = struct{}{}
	}
	selectedLocations := make([]domain.AssetLocation, 0)
	for _, location := range persisted.Locations {
		if _, selected := matchedAssets[location.AssetID]; selected {
			selectedLocations = append(selectedLocations, location)
		}
	}
	if err := verifyBoundAssets(persisted, matches, platform, format); err != nil {
		return BindingResult{}, err
	}
	return BindingResult{Graph: persisted, Availability: deriveAvailability(selectedLocations)}, nil
}

func (binder *InventoryBinder) buildGraph(ctx context.Context, metadata domain.MetadataSearchResult, platform, format string, matches []providers.InventoryMatch, existing WorkGraph, hasExisting bool) (WorkGraph, error) {
	work := domain.Work{
		Title:    metadata.Title,
		Medium:   metadata.Medium,
		WorkType: metadata.WorkType,
	}
	if hasExisting {
		work = existing.Work
	} else {
		id, err := newLocalWorkID()
		if err != nil {
			return WorkGraph{}, err
		}
		work.ID = id
	}

	edition := domain.Edition{
		ID:     domain.EditionID(stableCatalogID("edition", string(work.ID), platform, format)),
		WorkID: work.ID, Platform: platform, Format: format,
	}
	for _, existingEdition := range existing.Editions {
		if existingEdition.Platform == platform && existingEdition.Format == format {
			edition = existingEdition
			break
		}
	}
	editionID := edition.ID
	graph := WorkGraph{
		Work:     work,
		Editions: []domain.Edition{edition},
	}

	identity := domain.ExternalIdentity{
		ID:     domain.ExternalIdentityID(stableCatalogID("identity", metadata.Provider, metadata.ExternalID)),
		WorkID: work.ID, Provider: metadata.Provider, ExternalID: metadata.ExternalID,
	}
	for _, existingIdentity := range existing.ExternalIdentities {
		if existingIdentity.Provider == metadata.Provider && existingIdentity.ExternalID == metadata.ExternalID {
			identity = existingIdentity
			break
		}
	}
	graph.ExternalIdentities = []domain.ExternalIdentity{identity}

	seenAssets := make(map[domain.AssetID]struct{}, len(matches))
	consumedExistingParts := make(map[domain.AssetPartID]struct{}, len(existing.Parts))
	for _, match := range matches {
		if match.Platform != platform || match.Format != format {
			return WorkGraph{}, fmt.Errorf("inventory source returned an asset outside requested edition %q/%q", platform, format)
		}
		if match.AssetID == "" {
			return WorkGraph{}, errors.New("inventory source returned an asset without an ID")
		}
		if _, duplicate := seenAssets[match.AssetID]; duplicate {
			continue
		}
		seenAssets[match.AssetID] = struct{}{}
		asset := domain.Asset{
			ID: match.AssetID, EditionID: editionID, Kind: match.Format, TotalSizeBytes: match.TotalSizeBytes,
		}
		for _, existingAsset := range existing.Assets {
			if existingAsset.ID != asset.ID {
				continue
			}
			if existingAsset.EditionID != asset.EditionID || existingAsset.Kind != asset.Kind || existingAsset.TotalSizeBytes != asset.TotalSizeBytes {
				return WorkGraph{}, fmt.Errorf("catalog Asset %q conflicts with the exact inventory match", asset.ID)
			}
		}
		graph.Assets = append(graph.Assets, asset)
		for _, part := range stablePartsForAsset(match.AssetID, match.Parts) {
			matchedExisting := false
			indexedConflict := false
			for _, existingPart := range existing.Parts {
				if existingPart.AssetID != match.AssetID {
					continue
				}
				if _, consumed := consumedExistingParts[existingPart.ID]; consumed {
					continue
				}
				if sameAssetPart(existingPart, part) {
					consumedExistingParts[existingPart.ID] = struct{}{}
					matchedExisting = true
					break
				}
				if part.PartIndex != nil && existingPart.PartIndex != nil && *part.PartIndex == *existingPart.PartIndex {
					indexedConflict = true
				}
			}
			if indexedConflict && !matchedExisting {
				return WorkGraph{}, fmt.Errorf("catalog AssetPart for Asset %q has a conflicting part index", match.AssetID)
			}
			if !matchedExisting {
				graph.Parts = append(graph.Parts, part)
			}
		}

		locations, err := binder.storage.LocationsForAsset(ctx, match.AssetID)
		if err != nil {
			return WorkGraph{}, fmt.Errorf("load storage locations for Asset %q: %w", match.AssetID, err)
		}
		seenLocations := make(map[string]struct{}, len(locations))
		for _, location := range locations {
			if location.AssetID != "" && location.AssetID != match.AssetID {
				return WorkGraph{}, fmt.Errorf("storage source returned a location for another Asset instead of %q", match.AssetID)
			}
			location.AssetID = match.AssetID
			location.ID = domain.AssetLocationID(stableCatalogID("location", string(location.AssetID), location.StorageProviderID, location.Locator, location.LocationClass))
			if _, duplicate := seenLocations[string(location.ID)]; duplicate {
				continue
			}
			seenLocations[string(location.ID)] = struct{}{}
			if hasSameLocation(existing.Locations, location) {
				continue
			}
			graph.Locations = append(graph.Locations, location)
		}
	}
	return graph, nil
}

func sameAssetPart(first, second domain.AssetPart) bool {
	if first.AssetID != second.AssetID || first.Role != second.Role || first.Filename != second.Filename ||
		first.RelativePath != second.RelativePath || first.SizeBytes != second.SizeBytes {
		return false
	}
	if first.PartIndex == nil || second.PartIndex == nil {
		return first.PartIndex == nil && second.PartIndex == nil
	}
	return *first.PartIndex == *second.PartIndex
}

func hasSameLocation(locations []domain.AssetLocation, candidate domain.AssetLocation) bool {
	for _, location := range locations {
		if location.AssetID == candidate.AssetID && location.StorageProviderID == candidate.StorageProviderID &&
			location.Locator == candidate.Locator && location.LocationClass == candidate.LocationClass {
			return true
		}
	}
	return false
}

func stablePartsForAsset(assetID domain.AssetID, parts []providers.InventoryPart) []domain.AssetPart {
	result := make([]domain.AssetPart, 0, len(parts))
	occurrences := make(map[string]int, len(parts))
	for _, part := range parts {
		partIndex := ""
		if part.PartIndex != nil {
			partIndex = strconv.Itoa(*part.PartIndex)
		}
		signature := stableCatalogID("part-key", string(assetID), partIndex, part.Role, part.Filename, part.RelativePath, strconv.FormatInt(part.SizeBytes, 10))
		occurrence := occurrences[signature]
		occurrences[signature] = occurrence + 1
		result = append(result, domain.AssetPart{
			ID:      domain.AssetPartID(stableCatalogID("part", signature, strconv.Itoa(occurrence))),
			AssetID: assetID, PartIndex: cloneIndex(part.PartIndex), Role: part.Role, Filename: part.Filename,
			RelativePath: part.RelativePath, SizeBytes: part.SizeBytes,
		})
	}
	return result
}

func cloneIndex(index *int) *int {
	if index == nil {
		return nil
	}
	cloned := *index
	return &cloned
}

func stableCatalogID(prefix string, components ...string) string {
	hash := sha256.New()
	for _, component := range components {
		_, _ = fmt.Fprintf(hash, "%d:", len(component))
		_, _ = hash.Write([]byte(component))
	}
	return prefix + "-" + hex.EncodeToString(hash.Sum(nil))
}

func newLocalWorkID() (domain.WorkID, error) {
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return "", fmt.Errorf("generate local catalog Work ID: %w", err)
	}
	return domain.WorkID("work-" + hex.EncodeToString(randomID[:])), nil
}

func deriveAvailability(locations []domain.AssetLocation) Availability {
	// AssetLocation has no verification marker for local files, so a declared
	// local_cache locator cannot prove readiness. Archive-only remains honest.
	for _, location := range locations {
		if location.LocationClass == "archive" {
			return AvailabilityArchived
		}
	}
	return AvailabilityUnavailable
}

func verifyBoundAssets(graph WorkGraph, matches []providers.InventoryMatch, platform, format string) error {
	var editionID domain.EditionID
	for _, edition := range graph.Editions {
		if edition.Platform == platform && edition.Format == format {
			editionID = edition.ID
			break
		}
	}
	if editionID == "" {
		return errors.New("persisted catalog graph is missing the requested Edition")
	}
	assets := make(map[domain.AssetID]domain.EditionID, len(graph.Assets))
	for _, asset := range graph.Assets {
		assets[asset.ID] = asset.EditionID
	}
	for _, match := range matches {
		if linkedEdition, exists := assets[match.AssetID]; !exists || linkedEdition != editionID {
			return fmt.Errorf("persisted catalog graph is missing inventory Asset %q", match.AssetID)
		}
	}
	return nil
}
