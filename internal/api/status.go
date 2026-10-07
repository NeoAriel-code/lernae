// Package api exposes the small public REST surface required by Lernae.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"lernae/internal/agent"
	"lernae/internal/catalog"
	"lernae/internal/config"
	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/internal/providers/igdb"
	"lernae/internal/universe"
)

const (
	DatabasePingTimeout          = time.Second
	AgentStatusTimeout           = 700 * time.Millisecond
	maxInventoryResolveBodyBytes = 64 << 10
	inventoryNotConfiguredCode   = "inventory_not_configured"
	inventoryUnavailableCode     = "inventory_unavailable"
)

type ComponentStatus struct {
	Status string `json:"status"`
}

type SystemStatus struct {
	Server   ComponentStatus `json:"server"`
	Database ComponentStatus `json:"database"`
	Agent    ComponentStatus `json:"agent"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

type InventoryResolveRequest struct {
	WorkIdentity InventoryWorkIdentity `json:"work_identity"`
	Edition      InventoryEdition      `json:"edition"`
}

type InventoryWorkIdentity struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
}

type InventoryEdition struct {
	Platform string `json:"platform"`
	Format   string `json:"format"`
}

type InventoryResolveResponse struct {
	Owned        bool                 `json:"owned"`
	Availability catalog.Availability `json:"availability"`
	WorkID       domain.WorkID        `json:"work_id,omitempty"`
	EditionID    domain.EditionID     `json:"edition_id,omitempty"`
	AssetIDs     []domain.AssetID     `json:"asset_ids,omitempty"`
}

type StatusHandler struct {
	Database                 *sql.DB
	Agent                    agent.StatusClient
	Metadata                 providers.MetadataSource
	Search                   providers.UniversalSearchSource
	Details                  providers.WorkDetailsSource
	Inventory                *catalog.InventoryBinder
	Universes                *catalog.UniverseApplication
	Play                     PlayStarter
	PlayJobs                 PlayJobReader
	Acquisitions             AcquisitionService
	AcquisitionCandidates    AcquisitionCandidateService
	MetadataLanguageSettings MetadataLanguagePreference
	Logger                   *slog.Logger
}

type MetadataLanguagePreference interface {
	GetMetadataLanguage() (config.MetadataLanguage, error)
	SetMetadataLanguage(config.MetadataLanguage) error
}

func (handler StatusHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", handler.health)
	mux.HandleFunc("/api/v1/system/status", handler.systemStatus)
	mux.HandleFunc("/api/v1/settings/metadata-language", handler.metadataLanguageSettings)
	mux.HandleFunc("/api/v1/search", handler.search)
	mux.HandleFunc("/api/v1/work-details", handler.workDetails)
	mux.HandleFunc("/api/v1/inventory/resolve", handler.resolveInventory)
	mux.HandleFunc("/api/v1/universes", handler.universeCollection)
	mux.HandleFunc("/api/v1/universes/resolve", handler.resolveUniverse)
	mux.HandleFunc("/api/v1/universes/relationships/resolve", handler.resolveUniverseRelationships)
	mux.HandleFunc("/api/v1/universes/{id}", handler.getUniverse)
	mux.HandleFunc("/api/v1/universes/{id}/confirm", handler.confirmUniverse)
	mux.HandleFunc("/api/v1/universes/{id}/memberships", handler.confirmUniverseWork)
	mux.HandleFunc("/api/v1/universes/{id}/reject", handler.rejectUniverseWork)
	mux.HandleFunc("/api/v1/universes/{id}/reset", handler.resetUniverseDecision)
	mux.HandleFunc("/api/v1/play", handler.startPlay)
	mux.HandleFunc("/api/v1/play/{id}", handler.getPlay)
	mux.HandleFunc("/api/v1/acquisitions", handler.acquisitionCollection)
	mux.HandleFunc("/api/v1/acquisitions/{id}", handler.getAcquisition)
	mux.HandleFunc("/api/v1/acquisitions/{id}/candidates", handler.acquisitionCandidates)
	mux.HandleFunc("/api/v1/acquisitions/{id}/selection", handler.acquisitionSelection)
	return mux
}

type universeRelationshipResolveRequest struct {
	UniverseID string `json:"universe_id,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Language   string `json:"language,omitempty"`
}

func (handler StatusHandler) resolveUniverseRelationships(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe relationship service is not configured")
		return
	}
	var payload universeRelationshipResolveRequest
	if !decodeUniverseRequest(w, request, &payload) {
		return
	}
	hasID, hasAlias := strings.TrimSpace(payload.UniverseID) != "", strings.TrimSpace(payload.Alias) != ""
	if hasID == hasAlias || (hasID && !validBoundedInput(payload.UniverseID, 128)) ||
		(hasAlias && !validBoundedInput(payload.Alias, 200)) ||
		(payload.Language != "" && !validBoundedInput(payload.Language, 35)) {
		writeUniverseError(w, http.StatusBadRequest, "provide exactly one valid Universe ID or exact localized alias")
		return
	}
	result, err := handler.Universes.ResolveRelationships(request.Context(), domain.UniverseID(strings.TrimSpace(payload.UniverseID)),
		strings.TrimSpace(payload.Alias), strings.TrimSpace(payload.Language))
	if err != nil {
		status, message := universeRelationshipResolutionError(err)
		writeUniverseError(w, status, message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func universeRelationshipResolutionError(err error) (int, string) {
	switch {
	case errors.Is(err, catalog.ErrRelationshipAnchorInvalid):
		return http.StatusBadRequest, "invalid Universe relationship anchor"
	case errors.Is(err, catalog.ErrRelationshipAnchorNotFound):
		return http.StatusNotFound, "exact Universe relationship anchor was not found"
	case errors.Is(err, catalog.ErrRelationshipAnchorAmbiguous):
		return http.StatusConflict, "Universe relationship anchor is ambiguous"
	case errors.Is(err, catalog.ErrRelationshipResolutionUnavailable), errors.Is(err, catalog.ErrUniverseResolveBoundsExceeded),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "Universe relationship resolution is unavailable"
	default:
		return http.StatusBadGateway, "relationship metadata could not be safely resolved"
	}
}

type MetadataLanguageSettingsResponse struct {
	MetadataLanguage config.MetadataLanguage `json:"metadata_language"`
}

type MetadataLanguageSettingsRequest struct {
	MetadataLanguage config.MetadataLanguage `json:"metadata_language"`
}

func (handler StatusHandler) metadataLanguageSettings(w http.ResponseWriter, request *http.Request) {
	if handler.MetadataLanguageSettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metadata language settings are unavailable"})
		return
	}
	switch request.Method {
	case http.MethodGet:
		language, err := handler.MetadataLanguageSettings.GetMetadataLanguage()
		if err != nil || !language.Valid() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metadata language settings are unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, MetadataLanguageSettingsResponse{MetadataLanguage: language})
	case http.MethodPatch:
		var body MetadataLanguageSettingsRequest
		if err := decodeMetadataLanguageSettingsRequest(w, request, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid metadata language settings"})
			return
		}
		if !body.MetadataLanguage.Valid() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported metadata language"})
			return
		}
		if err := handler.MetadataLanguageSettings.SetMetadataLanguage(body.MetadataLanguage); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metadata language settings could not be saved"})
			return
		}
		writeJSON(w, http.StatusOK, MetadataLanguageSettingsResponse{MetadataLanguage: body.MetadataLanguage})
	default:
		w.Header().Set("Allow", "GET, PATCH")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func decodeMetadataLanguageSettingsRequest(w http.ResponseWriter, request *http.Request, destination *MetadataLanguageSettingsRequest) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("request must be JSON")
	}
	request.Body = http.MaxBytesReader(w, request.Body, 4096)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func (handler StatusHandler) health(w http.ResponseWriter, request *http.Request) {
	if !getOnly(w, request) {
		return
	}
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

func (handler StatusHandler) systemStatus(w http.ResponseWriter, request *http.Request) {
	if !getOnly(w, request) {
		return
	}
	status := SystemStatus{
		Server:   ComponentStatus{Status: "online"},
		Database: ComponentStatus{Status: "unavailable"},
		Agent:    ComponentStatus{Status: "offline"},
	}
	if handler.Database != nil {
		ctx, cancel := context.WithTimeout(request.Context(), DatabasePingTimeout)
		err := handler.Database.PingContext(ctx)
		cancel()
		if err == nil {
			status.Database.Status = "ready"
		}
	}
	if handler.Agent != nil {
		ctx, cancel := context.WithTimeout(request.Context(), AgentStatusTimeout)
		agentStatus, err := handler.Agent.GetStatus(ctx)
		cancel()
		if err == nil && agentStatus.State == agent.StateOnline {
			status.Agent.Status = "connected"
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (handler StatusHandler) search(w http.ResponseWriter, request *http.Request) {
	if !getOnly(w, request) {
		return
	}
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "search query must not be empty"})
		return
	}
	if len([]byte(query)) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid search query"})
		return
	}
	limit := 20
	if values, present := request.URL.Query()["limit"]; present {
		if len(values) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid search limit"})
			return
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(values[0]))
		if err != nil || parsed < 1 || parsed > universe.MaxCandidateResults {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid search limit"})
			return
		}
		limit = parsed
	}
	if handler.Search == nil && handler.Metadata == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metadata search is not configured"})
		return
	}
	if handler.Search != nil {
		result, err := handler.Search.Search(request.Context(), query, limit)
		if err != nil {
			status, message := universalSearchError(err)
			writeJSON(w, status, map[string]string{"error": message})
			return
		}
		if result.Results == nil {
			result.Results = []domain.MetadataSearchResult{}
		}
		if result.Sources == nil {
			result.Sources = []domain.SearchSourceStatus{}
		}
		handler.attachAutomaticUniverseDiscovery(request.Context(), query, &result)
		writeJSON(w, http.StatusOK, result)
		return
	}
	results, err := handler.Metadata.SearchWorks(request.Context(), query)
	if err != nil {
		status, message := metadataSearchError(err)
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	if results == nil {
		results = []domain.MetadataSearchResult{}
	}
	state := domain.SearchSourceEmpty
	if len(results) > 0 {
		state = domain.SearchSourceAvailable
	}
	provider := "metadata"
	if len(results) > 0 {
		provider = results[0].Provider
	}
	response := domain.SearchResponse{
		Results: results,
		Sources: []domain.SearchSourceStatus{{Provider: provider, State: state, ResultCount: len(results)}},
	}
	handler.attachAutomaticUniverseDiscovery(request.Context(), query, &response)
	writeJSON(w, http.StatusOK, response)
}

func (handler StatusHandler) attachAutomaticUniverseDiscovery(ctx context.Context, query string, response *domain.SearchResponse) {
	if handler.Universes == nil {
		return
	}
	results := response.Results
	if len(results) > universe.MaxCandidateResults {
		results = results[:universe.MaxCandidateResults]
	}
	discovery, err := handler.Universes.AutoDiscover(ctx, query, results)
	if err != nil {
		logger := handler.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "automatic Universe discovery unavailable", "category", universeDiscoveryErrorCategory(err))
		response.UniverseDiscovery = nil
		return
	}
	response.UniverseDiscovery = &discovery
}

func universeDiscoveryErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, catalog.ErrUniverseResolveBoundsExceeded):
		return "bounds_exceeded"
	default:
		return "catalog_unavailable"
	}
}

func universalSearchError(err error) (int, string) {
	switch {
	case errors.Is(err, providers.ErrSearchInvalid):
		return http.StatusBadRequest, "invalid search query"
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, "search was canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "search is temporarily unavailable"
	default:
		return http.StatusInternalServerError, "search failed"
	}
}

func (handler StatusHandler) workDetails(w http.ResponseWriter, request *http.Request) {
	if !getOnly(w, request) {
		return
	}
	query := request.URL.Query()
	providerValues := query["provider"]
	externalIDValues := query["external_id"]
	if len(providerValues) != 1 || len(externalIDValues) != 1 ||
		!nonEmptyBounded(providerValues[0], 128) || !nonEmptyBounded(externalIDValues[0], 512) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid work identity"})
		return
	}
	if handler.Details == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metadata details are not configured"})
		return
	}
	details, err := handler.Details.GetWorkDetails(request.Context(), providerValues[0], externalIDValues[0])
	if err != nil {
		status, message := workDetailsError(err)
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	if details.Provider == "" || details.ExternalID == "" || details.Title == "" || !details.Medium.Valid() || details.WorkType == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "metadata provider returned invalid work details"})
		return
	}
	if details.PlatformCandidates == nil {
		details.PlatformCandidates = []domain.PlatformCandidate{}
	}
	if handler.Universes != nil {
		// Series identity is local persisted graph data, never provider-supplied
		// display metadata. Replace any source value with the exact catalog read.
		details.Series = nil
		details.SeriesTruncated = false
		series, seriesTruncated, err := handler.Universes.GetWorkSeriesDetails(
			request.Context(), providerValues[0], externalIDValues[0],
		)
		if err != nil {
			logger := handler.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.WarnContext(request.Context(), "local Series Details unavailable", "category", "catalog_unavailable")
		} else if len(series) > 0 {
			details.Series = series
			details.SeriesTruncated = seriesTruncated
		}
	}
	writeJSON(w, http.StatusOK, details)
}

func workDetailsError(err error) (int, string) {
	switch {
	case errors.Is(err, providers.ErrDetailsInvalidExternalID):
		return http.StatusBadRequest, "invalid work identity"
	case errors.Is(err, providers.ErrDetailsUnsupportedProvider):
		return http.StatusBadRequest, "unsupported metadata provider"
	case errors.Is(err, providers.ErrDetailsWorkNotFound):
		return http.StatusNotFound, "work was not found"
	case errors.Is(err, providers.ErrDetailsProviderUnavailable), errors.Is(err, providers.ErrDetailsRateLimited),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "metadata provider is temporarily unavailable"
	case errors.Is(err, providers.ErrDetailsMalformedResponse), errors.Is(err, providers.ErrDetailsResponseTooLarge):
		return http.StatusBadGateway, "metadata provider returned invalid work details"
	case errors.Is(err, igdb.ErrInvalidExternalID):
		return http.StatusBadRequest, "invalid work identity"
	case errors.Is(err, igdb.ErrUnsupportedProvider):
		return http.StatusBadRequest, "unsupported metadata provider"
	case errors.Is(err, igdb.ErrWorkNotFound):
		return http.StatusNotFound, "work was not found"
	case errors.Is(err, igdb.ErrConfiguration):
		return http.StatusServiceUnavailable, "metadata details are not configured"
	case errors.Is(err, igdb.ErrAuthentication), errors.Is(err, igdb.ErrUnauthorized):
		return http.StatusBadGateway, "metadata provider authentication failed"
	case errors.Is(err, igdb.ErrRateLimited), errors.Is(err, igdb.ErrProviderUnavailable),
		errors.Is(err, igdb.ErrNetwork), errors.Is(err, igdb.ErrTimeout), errors.Is(err, igdb.ErrCanceled):
		return http.StatusServiceUnavailable, "metadata provider is temporarily unavailable"
	case errors.Is(err, igdb.ErrProviderRequest), errors.Is(err, igdb.ErrMalformedResponse), errors.Is(err, igdb.ErrResponseTooLarge):
		return http.StatusBadGateway, "metadata provider returned invalid work details"
	default:
		return http.StatusBadGateway, "metadata details are temporarily unavailable"
	}
}

func (handler StatusHandler) resolveInventory(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request must be JSON"})
		return
	}
	var payload InventoryResolveRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxInventoryResolveBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid inventory resolution request"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid inventory resolution request"})
		return
	}
	if !validInventoryResolveRequest(payload) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid inventory resolution request"})
		return
	}
	if handler.Inventory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"code": inventoryNotConfiguredCode, "error": "inventory resolution is not configured",
		})
		return
	}
	identity := providers.ExternalWorkIdentity{
		Provider: payload.WorkIdentity.Provider, ExternalID: payload.WorkIdentity.ExternalID,
	}
	result, err := handler.Inventory.Resolve(request.Context(), identity, payload.Edition.Platform, payload.Edition.Format, handler.Details)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"code": inventoryNotConfiguredCode, "error": "inventory Manifest file is unavailable",
			})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"code": inventoryUnavailableCode, "error": "inventory resolution is temporarily unavailable",
		})
		return
	}
	response := InventoryResolveResponse{Availability: result.Availability}
	if result.Graph.Work.ID != "" {
		response.WorkID = result.Graph.Work.ID
		for _, edition := range result.Graph.Editions {
			if edition.Platform != payload.Edition.Platform || edition.Format != payload.Edition.Format {
				continue
			}
			response.EditionID = edition.ID
			for _, asset := range result.Graph.Assets {
				if asset.EditionID == edition.ID {
					response.AssetIDs = append(response.AssetIDs, asset.ID)
				}
			}
			break
		}
		response.Owned = len(response.AssetIDs) > 0
		if !response.Owned {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "inventory resolution is temporarily unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func validInventoryResolveRequest(payload InventoryResolveRequest) bool {
	return nonEmptyBounded(payload.WorkIdentity.Provider, 128) &&
		nonEmptyBounded(payload.WorkIdentity.ExternalID, 512) &&
		nonEmptyBounded(payload.Edition.Platform, 256) &&
		nonEmptyBounded(payload.Edition.Format, 128)
}

func nonEmptyBounded(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit
}

func metadataSearchError(err error) (int, string) {
	switch {
	case errors.Is(err, igdb.ErrInvalidQuery):
		return http.StatusBadRequest, "invalid search query"
	case errors.Is(err, igdb.ErrConfiguration):
		return http.StatusServiceUnavailable, "metadata search is not configured"
	case errors.Is(err, igdb.ErrAuthentication), errors.Is(err, igdb.ErrUnauthorized):
		return http.StatusBadGateway, "metadata provider authentication failed"
	case errors.Is(err, igdb.ErrRateLimited), errors.Is(err, igdb.ErrProviderUnavailable),
		errors.Is(err, igdb.ErrNetwork), errors.Is(err, igdb.ErrTimeout), errors.Is(err, igdb.ErrCanceled):
		return http.StatusServiceUnavailable, "metadata provider is temporarily unavailable"
	case errors.Is(err, igdb.ErrProviderRequest), errors.Is(err, igdb.ErrMalformedResponse), errors.Is(err, igdb.ErrResponseTooLarge):
		return http.StatusBadGateway, "metadata provider returned an invalid response"
	default:
		return http.StatusInternalServerError, "metadata search failed"
	}
}

func getOnly(w http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", http.MethodGet)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
