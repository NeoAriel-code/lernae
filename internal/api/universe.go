package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/catalog"
	"lernae/internal/domain"
	"lernae/internal/universe"
)

const maxUniverseBodyBytes = 64 << 10

type universeWorkInput struct {
	Provider   string        `json:"provider"`
	ExternalID string        `json:"external_id"`
	Medium     domain.Medium `json:"medium"`
	WorkType   string        `json:"work_type"`
	Title      string        `json:"title"`
}

func (input universeWorkInput) materialization() domain.WorkMaterialization {
	return domain.WorkMaterialization{Provider: input.Provider, ExternalID: input.ExternalID, Medium: input.Medium, WorkType: input.WorkType, Title: input.Title}
}

type universeCreateRequest struct {
	OperationID domain.UniverseOperationID `json:"operation_id"`
	Title       string                     `json:"title"`
	SortTitle   string                     `json:"sort_title"`
	Description string                     `json:"description"`
	Works       []universeWorkInput        `json:"works"`
}

type universeResolveRequest struct {
	Query   string              `json:"query"`
	Results []universeWorkInput `json:"results"`
}

type universeRejectRequest struct {
	Work   universeWorkInput `json:"work"`
	Reason string            `json:"reason"`
}

type universeRemoveRequest struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason"`
}

type universeResetRequest struct {
	Action     catalog.UniverseResetAction `json:"action"`
	Provider   string                      `json:"provider"`
	ExternalID string                      `json:"external_id"`
}

type universeSummaryDTO struct {
	ID                    string    `json:"id"`
	Title                 string    `json:"title"`
	SortTitle             *string   `json:"sort_title"`
	Description           *string   `json:"description"`
	Artwork               *string   `json:"artwork"`
	ExistenceConfidence   float64   `json:"existence_confidence"`
	NamingConfidence      float64   `json:"naming_confidence"`
	NamingConfidenceLevel string    `json:"naming_confidence_level"`
	NamingEvidence        string    `json:"naming_evidence"`
	Provenance            string    `json:"provenance"`
	ConfirmedByUser       bool      `json:"confirmed_by_user"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type universeSearchResponse struct {
	Universes []universeSummaryDTO `json:"universes"`
}

type universeMembershipDTO struct {
	Provider        string  `json:"provider,omitempty"`
	ExternalID      string  `json:"external_id,omitempty"`
	Title           string  `json:"title"`
	Medium          string  `json:"medium"`
	MediaType       string  `json:"media_type"`
	WorkType        string  `json:"work_type"`
	Provenance      string  `json:"provenance"`
	Confidence      float64 `json:"confidence"`
	Evidence        string  `json:"evidence"`
	Reason          string  `json:"reason"`
	ConfirmedByUser bool    `json:"confirmed_by_user"`
}

type universeExclusionDTO struct {
	Provider   string `json:"provider,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Title      string `json:"title"`
	Medium     string `json:"medium"`
	WorkType   string `json:"work_type"`
	Reason     string `json:"reason"`
}

type universeAliasDTO struct {
	Value           string  `json:"value"`
	Language        *string `json:"language"`
	Provenance      string  `json:"provenance"`
	Confidence      float64 `json:"confidence"`
	ConfirmedByUser bool    `json:"confirmed_by_user"`
}

type universeDetailResponse struct {
	ID                    string                  `json:"id"`
	Title                 string                  `json:"title"`
	SortTitle             *string                 `json:"sort_title"`
	Description           *string                 `json:"description"`
	Artwork               *string                 `json:"artwork"`
	ExistenceConfidence   float64                 `json:"existence_confidence"`
	NamingConfidence      float64                 `json:"naming_confidence"`
	NamingConfidenceLevel string                  `json:"naming_confidence_level"`
	NamingEvidence        string                  `json:"naming_evidence"`
	Provenance            string                  `json:"provenance"`
	ConfirmedByUser       bool                    `json:"confirmed_by_user"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
	Aliases               []universeAliasDTO      `json:"aliases"`
	Memberships           []universeMembershipDTO `json:"memberships"`
	Suggestions           []universeMembershipDTO `json:"suggestions"`
	Exclusions            []universeExclusionDTO  `json:"exclusions"`
	MembershipsTruncated  bool                    `json:"memberships_truncated"`
	AliasesTruncated      bool                    `json:"aliases_truncated"`
	SuggestionsTruncated  bool                    `json:"suggestions_truncated"`
	ExclusionsTruncated   bool                    `json:"exclusions_truncated"`
}

type universeProposalDTO struct {
	UniverseID                   string  `json:"universe_id,omitempty"`
	UniverseTitle                string  `json:"universe_title"`
	Provider                     string  `json:"provider"`
	ExternalID                   string  `json:"external_id"`
	MediaType                    string  `json:"media_type"`
	Title                        string  `json:"title"`
	ExistingMembershipUniverseID string  `json:"existing_membership_universe_id,omitempty"`
	ExistingState                string  `json:"existing_state"`
	ConfirmedByUser              bool    `json:"confirmed_by_user"`
	Evidence                     string  `json:"evidence"`
	Reason                       string  `json:"reason"`
	Confidence                   float64 `json:"confidence"`
	ConfidenceLevel              string  `json:"confidence_level"`
	Suppressed                   bool    `json:"suppressed"`
	RequiresConfirmation         bool    `json:"requires_confirmation"`
}

type universeCandidateDTO struct {
	ID                    string                `json:"id,omitempty"`
	Title                 string                `json:"title"`
	Existing              bool                  `json:"existing"`
	Evidence              string                `json:"evidence"`
	Reason                string                `json:"reason"`
	ExistenceConfidence   float64               `json:"existence_confidence"`
	NamingConfidence      float64               `json:"naming_confidence"`
	NamingConfidenceLevel string                `json:"naming_confidence_level"`
	NamingEvidence        string                `json:"naming_evidence"`
	Confidence            float64               `json:"confidence"`
	ConfidenceLevel       string                `json:"confidence_level"`
	Memberships           []universeProposalDTO `json:"memberships"`
}

type universeResolveResponse struct {
	Candidates []universeCandidateDTO `json:"candidates"`
}

func (handler StatusHandler) universeCollection(w http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		handler.searchUniverses(w, request)
	case http.MethodPost:
		handler.createUniverse(w, request)
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler StatusHandler) searchUniverses(w http.ResponseWriter, request *http.Request) {
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	queryValues := request.URL.Query()["q"]
	if len(queryValues) != 1 || !validBoundedInput(queryValues[0], 200) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe search query")
		return
	}
	limit := 20
	if values, present := request.URL.Query()["limit"]; present {
		if len(values) != 1 {
			writeUniverseError(w, http.StatusBadRequest, "invalid Universe search limit")
			return
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 20 {
			writeUniverseError(w, http.StatusBadRequest, "invalid Universe search limit")
			return
		}
		limit = parsed
	}
	results, err := handler.Universes.SearchUniverses(request.Context(), strings.TrimSpace(queryValues[0]), limit)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	response := universeSearchResponse{Universes: make([]universeSummaryDTO, 0, len(results))}
	for _, result := range results {
		response.Universes = append(response.Universes, universeSummaryDTO{
			ID: string(result.ID), Title: result.Title, SortTitle: result.SortTitle,
			Description: result.Description, Artwork: result.Artwork,
			ExistenceConfidence: safeConfidence(result.ExistenceConfidence), NamingConfidence: safeConfidence(result.NamingConfidence),
			NamingConfidenceLevel: safeDomainNamingConfidenceLevel(result.NamingConfidenceLevel), NamingEvidence: safeNamingEvidence(result.NamingEvidence),
			Provenance: safeUniverseRecordProvenance(result.Provenance), ConfirmedByUser: result.ConfirmedByUser,
			CreatedAt: result.CreatedAt, UpdatedAt: result.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (handler StatusHandler) resolveUniverse(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	var payload universeResolveRequest
	if !decodeUniverseRequest(w, request, &payload) {
		return
	}
	if !validBoundedInput(payload.Query, 200) || len(payload.Results) > universe.MaxCandidateResults {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe resolution request")
		return
	}
	results := make([]domain.MetadataSearchResult, 0, len(payload.Results))
	for _, input := range payload.Results {
		if !validUniverseWorkInput(input) {
			writeUniverseError(w, http.StatusBadRequest, "invalid Universe resolution result")
			return
		}
		results = append(results, domain.MetadataSearchResult{
			Provider: input.Provider, ExternalID: input.ExternalID, Title: input.Title,
			Medium: input.Medium, WorkType: input.WorkType, MediaType: safeWorkMediaType(input.Medium, input.WorkType),
		})
	}
	candidates, err := handler.Universes.Resolve(request.Context(), strings.TrimSpace(payload.Query), results)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	response := universeResolveResponse{Candidates: make([]universeCandidateDTO, 0, len(candidates))}
	for _, candidate := range candidates {
		evidence, reason := safeUniverseEvidence(candidate.Evidence)
		item := universeCandidateDTO{
			ID: string(candidate.UniverseID), Title: candidate.Title, Existing: candidate.Existing,
			Evidence: evidence, Reason: reason, ExistenceConfidence: safeConfidence(candidate.ExistenceConfidence),
			NamingConfidence: safeConfidence(candidate.NamingConfidence), NamingConfidenceLevel: safeConfidenceLevel(candidate.NamingConfidenceLevel),
			NamingEvidence: safeNamingEvidence(candidate.NamingEvidence), Confidence: safeConfidence(candidate.Confidence),
			ConfidenceLevel: safeConfidenceLevel(candidate.ConfidenceLevel), Memberships: make([]universeProposalDTO, 0, len(candidate.Memberships)),
		}
		for _, proposal := range candidate.Memberships {
			proposalEvidence, proposalReason := safeUniverseEvidence(proposal.Evidence)
			universeTitle := strings.TrimSpace(payload.Query)
			if candidate.Existing {
				universeTitle = proposal.UniverseTitle
				if universeTitle == "" {
					universeTitle = candidate.Title
				}
			}
			item.Memberships = append(item.Memberships, universeProposalDTO{
				UniverseID: string(proposal.UniverseID), UniverseTitle: universeTitle,
				Provider: proposal.Provider, ExternalID: proposal.ExternalID, MediaType: proposal.MediaType, Title: proposal.Title,
				ExistingMembershipUniverseID: string(proposal.ExistingMembershipUniverseID), ExistingState: safeExistingState(proposal.ExistingState),
				ConfirmedByUser: proposal.ConfirmedByUser, Evidence: proposalEvidence, Reason: proposalReason,
				Confidence: safeConfidence(proposal.Confidence), ConfidenceLevel: safeConfidenceLevel(proposal.ConfidenceLevel),
				Suppressed: proposal.Suppressed, RequiresConfirmation: proposal.RequiresConfirmation,
			})
		}
		response.Candidates = append(response.Candidates, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (handler StatusHandler) createUniverse(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	var payload universeCreateRequest
	if !decodeUniverseRequest(w, request, &payload) {
		return
	}
	if !validBoundedInput(string(payload.OperationID), 128) || !validBoundedInput(payload.Title, 512) ||
		!validOptionalPlainTextInput(payload.SortTitle, 512) || !validOptionalPlainTextInput(payload.Description, 2048) ||
		len(payload.Works) == 0 || len(payload.Works) > 20 {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe create request")
		return
	}
	works := make([]domain.WorkMaterialization, 0, len(payload.Works))
	seen := make(map[string]struct{}, len(payload.Works))
	for _, input := range payload.Works {
		if !validUniverseWorkInput(input) {
			writeUniverseError(w, http.StatusBadRequest, "invalid selected Work identity or display metadata")
			return
		}
		key := input.Provider + "\x00" + input.ExternalID
		if _, duplicate := seen[key]; duplicate {
			writeUniverseError(w, http.StatusBadRequest, "duplicate exact Work identity")
			return
		}
		seen[key] = struct{}{}
		works = append(works, input.materialization())
	}
	universe := domain.Universe{Title: payload.Title}
	if payload.SortTitle != "" {
		universe.SortTitle = &payload.SortTitle
	}
	if payload.Description != "" {
		universe.Description = &payload.Description
	}
	result, err := handler.Universes.CreateAndConfirm(request.Context(), payload.OperationID, universe, works)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, universeDetailToDTO(result.Detail))
}

func (handler StatusHandler) getUniverse(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	id := domain.UniverseID(request.PathValue("id"))
	if !validBoundedInput(string(id), 128) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe ID")
		return
	}
	detail, err := handler.Universes.GetUniverse(request.Context(), id)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, universeDetailToDTO(detail))
}

func (handler StatusHandler) confirmUniverse(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	if !validUniversePathID(request) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe ID")
		return
	}
	detail, err := handler.Universes.ConfirmUniverse(request.Context(), domain.UniverseID(request.PathValue("id")))
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, universeDetailToDTO(detail))
}

func (handler StatusHandler) confirmUniverseWork(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodDelete {
		handler.removeUniverseWork(w, request)
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	var input universeWorkInput
	if !decodeUniverseRequest(w, request, &input) {
		return
	}
	if !validUniverseWorkInput(input) {
		writeUniverseError(w, http.StatusBadRequest, "invalid exact Work identity or display metadata")
		return
	}
	if !validUniversePathID(request) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe ID")
		return
	}
	result, err := handler.Universes.ConfirmWork(request.Context(), domain.UniverseID(request.PathValue("id")), input.materialization())
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, universeDetailToDTO(result.Detail))
}

func (handler StatusHandler) removeUniverseWork(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	var payload universeRemoveRequest
	if !decodeUniverseRequest(w, request, &payload) {
		return
	}
	if !validBoundedInput(payload.Provider, 128) || !validBoundedInput(payload.ExternalID, 512) || !validBoundedInput(payload.Reason, 512) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe membership removal")
		return
	}
	if !validUniversePathID(request) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe ID")
		return
	}
	detail, err := handler.Universes.RemoveWork(request.Context(), domain.UniverseID(request.PathValue("id")), payload.Provider, payload.ExternalID, payload.Reason)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, universeDetailToDTO(detail))
}

func (handler StatusHandler) rejectUniverseWork(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	var payload universeRejectRequest
	if !decodeUniverseRequest(w, request, &payload) {
		return
	}
	if !validUniverseWorkInput(payload.Work) || !validBoundedInput(payload.Reason, 512) {
		writeUniverseError(w, http.StatusBadRequest, "invalid rejected Work decision")
		return
	}
	if !validUniversePathID(request) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe ID")
		return
	}
	detail, err := handler.Universes.RejectWork(request.Context(), domain.UniverseID(request.PathValue("id")), payload.Work.materialization(), payload.Reason)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, universeDetailToDTO(detail))
}

func (handler StatusHandler) resetUniverseDecision(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if handler.Universes == nil {
		writeUniverseError(w, http.StatusServiceUnavailable, "Universe service is not configured")
		return
	}
	var payload universeResetRequest
	if !decodeUniverseRequest(w, request, &payload) {
		return
	}
	if !validBoundedInput(payload.Provider, 128) || !validBoundedInput(payload.ExternalID, 512) ||
		(payload.Action != catalog.UniverseResetClearExclusion && payload.Action != catalog.UniverseResetClearConfirmation) {
		writeUniverseError(w, http.StatusBadRequest, "invalid typed Universe reset request")
		return
	}
	if !validUniversePathID(request) {
		writeUniverseError(w, http.StatusBadRequest, "invalid Universe ID")
		return
	}
	detail, err := handler.Universes.ResetDecision(request.Context(), domain.UniverseID(request.PathValue("id")), payload.Provider, payload.ExternalID, payload.Action)
	if err != nil {
		writeUniverseServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, universeDetailToDTO(detail))
}

func decodeUniverseRequest(w http.ResponseWriter, request *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeUniverseError(w, http.StatusUnsupportedMediaType, "request must use application/json")
		return false
	}
	request.Body = http.MaxBytesReader(w, request.Body, maxUniverseBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeUniverseError(w, http.StatusRequestEntityTooLarge, "request body exceeds the allowed size")
		} else {
			writeUniverseError(w, http.StatusBadRequest, "invalid JSON request")
		}
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeUniverseError(w, http.StatusBadRequest, "request body must contain exactly one JSON value")
		return false
	}
	return true
}

func validUniverseWorkInput(input universeWorkInput) bool {
	return validBoundedInput(input.Provider, 128) && validBoundedInput(input.ExternalID, 512) &&
		validBoundedInput(input.Title, 512) && validBoundedInput(input.WorkType, 128) && input.Medium.Valid()
}

func safeWorkMediaType(medium domain.Medium, workType string) string {
	switch {
	case medium == domain.MediumGame && workType == "game":
		return "game"
	case medium == domain.MediumLiterature && workType == "book":
		return "book"
	case medium == domain.MediumVideo && (workType == "series" || workType == "tv"):
		return "tv"
	default:
		return string(medium)
	}
}

func validUniversePathID(request *http.Request) bool {
	return validBoundedInput(request.PathValue("id"), 128)
}

func validBoundedInput(value string, maximum int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maximum {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validOptionalPlainTextInput(value string, maximum int) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && !unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func universeDetailToDTO(detail catalog.UniverseDetail) universeDetailResponse {
	response := universeDetailResponse{
		ID: string(detail.Universe.ID), Title: detail.Universe.Title,
		SortTitle: detail.Universe.SortTitle, Description: detail.Universe.Description, Artwork: detail.Universe.Artwork,
		ExistenceConfidence: safeConfidence(detail.Universe.ExistenceConfidence), NamingConfidence: safeConfidence(detail.Universe.NamingConfidence),
		NamingConfidenceLevel: safeDomainNamingConfidenceLevel(detail.Universe.NamingConfidenceLevel),
		NamingEvidence:        safeNamingEvidence(detail.Universe.NamingEvidence),
		Provenance:            safeUniverseRecordProvenance(detail.Universe.Provenance), ConfirmedByUser: detail.Universe.ConfirmedByUser,
		CreatedAt: detail.Universe.CreatedAt, UpdatedAt: detail.Universe.UpdatedAt,
		Aliases: make([]universeAliasDTO, 0, len(detail.Aliases)), AliasesTruncated: detail.AliasesTruncated,
		Memberships:          make([]universeMembershipDTO, 0, len(detail.Memberships)),
		Suggestions:          make([]universeMembershipDTO, 0, len(detail.Suggestions)),
		Exclusions:           make([]universeExclusionDTO, 0, len(detail.Exclusions)),
		MembershipsTruncated: detail.MembershipsTruncated, SuggestionsTruncated: detail.SuggestionsTruncated,
		ExclusionsTruncated: detail.ExclusionsTruncated,
	}
	for _, alias := range detail.Aliases {
		response.Aliases = append(response.Aliases, universeAliasDTO{
			Value: alias.Alias, Language: alias.Language,
			Provenance: safeUniverseProvenance(domain.UniverseMembershipProvenance(alias.Provenance)),
			Confidence: safeConfidence(alias.Confidence), ConfirmedByUser: alias.ConfirmedByUser,
		})
	}
	appendMembership := func(membership catalog.UniverseWorkRecord) universeMembershipDTO {
		provenance := safeUniverseProvenance(membership.Provenance)
		evidence, reason := safePersistedMembershipEvidence(membership.Evidence, membership.Reason)
		return universeMembershipDTO{
			Provider: membership.Provider, ExternalID: membership.ExternalID, Title: membership.Title,
			Medium: string(membership.Medium), MediaType: safeWorkMediaType(membership.Medium, membership.WorkType),
			WorkType: membership.WorkType, Provenance: provenance, Confidence: safeConfidence(membership.Confidence),
			Evidence: evidence, Reason: reason, ConfirmedByUser: membership.ConfirmedByUser,
		}
	}
	for _, membership := range detail.Memberships {
		response.Memberships = append(response.Memberships, appendMembership(membership))
	}
	for _, suggestion := range detail.Suggestions {
		response.Suggestions = append(response.Suggestions, appendMembership(suggestion))
	}
	for _, exclusion := range detail.Exclusions {
		response.Exclusions = append(response.Exclusions, universeExclusionDTO{
			Provider: exclusion.Provider, ExternalID: exclusion.ExternalID, Title: exclusion.Title,
			Medium: string(exclusion.Medium), WorkType: exclusion.WorkType, Reason: safeExclusionReason(exclusion.Reason),
		})
	}
	return response
}

func safeUniverseEvidence(evidence universe.EvidenceType) (string, string) {
	switch evidence {
	case universe.EvidenceConfirmedIdentity:
		return string(evidence), "This exact provider identity already has a user-confirmed membership in the Universe."
	case universe.EvidenceUserConfirmedMembership:
		return string(evidence), "The user explicitly accepted this exact Work for the Universe."
	case universe.EvidenceExistingMembership:
		return string(evidence), "This exact provider identity already has an accepted membership in the Universe."
	case universe.EvidenceExactTitleAnchor:
		return string(evidence), "The normalized user query exactly matches the result title and supplies this candidate's title anchor."
	case universe.EvidenceExplicitAlias:
		return string(evidence), "The normalized user query matches a stored Universe alias present as a whole-token title phrase."
	case universe.EvidenceDistinctiveToken:
		return string(evidence), "A distinctive normalized query token appears as a whole title token; no substring match is used."
	case universe.EvidenceWholePhraseAnchor:
		return string(evidence), "The normalized query appears as a complete title phrase; no substring match is used."
	case universe.EvidenceExplicitExclusion:
		return string(evidence), "An explicit exclusion blocks this exact Work/Universe pair from being proposed again."
	default:
		return "unknown", "No supported matching evidence was found."
	}
}

func safePersistedMembershipEvidence(value, reason string) (string, string) {
	evidence := universe.EvidenceType(value)
	_, supported := safeUniverseEvidence(evidence)
	if evidence == "" || supported == "No supported matching evidence was found." {
		return "unknown", "No supported matching evidence was found."
	}
	if validBoundedInput(reason, 512) {
		return string(evidence), reason
	}
	return safeUniverseEvidence(evidence)
}

func safeDomainNamingConfidenceLevel(level domain.UniverseNamingConfidenceLevel) string {
	switch level {
	case domain.UniverseNamingConfidenceNone, domain.UniverseNamingConfidenceModerate, domain.UniverseNamingConfidenceHigh:
		return string(level)
	default:
		return string(domain.UniverseNamingConfidenceNone)
	}
}

func safeNamingEvidence(evidence domain.UniverseNamingEvidence) string {
	switch evidence {
	case domain.UniverseNamingEvidenceConfirmedTitle, domain.UniverseNamingEvidenceConfirmedAlias,
		domain.UniverseNamingEvidenceDistinctiveQuery, domain.UniverseNamingEvidenceSharedAnchor,
		domain.UniverseNamingEvidenceUserAuthored, domain.UniverseNamingEvidenceLegacyUnknown:
		return string(evidence)
	default:
		return string(domain.UniverseNamingEvidenceLegacyUnknown)
	}
}

func safeExistingState(state universe.MembershipState) string {
	switch state {
	case universe.MembershipNone, universe.MembershipAccepted, universe.MembershipAcceptedElsewhere, universe.MembershipExcluded:
		return string(state)
	default:
		return string(universe.MembershipNone)
	}
}

func safeConfidenceLevel(level universe.ConfidenceLevel) string {
	switch level {
	case universe.ConfidenceNone, universe.ConfidenceModerate, universe.ConfidenceHigh:
		return string(level)
	default:
		return string(universe.ConfidenceNone)
	}
}

func safeConfidence(value float64) float64 {
	if value < 0 || value > 1 {
		return 0
	}
	return value
}

func safeUniverseProvenance(provenance domain.UniverseMembershipProvenance) string {
	if provenance.Valid() {
		return string(provenance)
	}
	return "unknown"
}

func safeUniverseRecordProvenance(provenance domain.UniverseProvenance) string {
	if provenance.Valid() {
		return string(provenance)
	}
	return "unknown"
}

func safeExclusionReason(_ string) string {
	return "manual exclusion"
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeUniverseError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeUniverseServiceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, catalog.ErrUniverseNotFound), errors.Is(err, catalog.ErrNotFound), errors.Is(err, catalog.ErrUniverseMembershipNotFound), errors.Is(err, catalog.ErrUniverseDecisionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, catalog.ErrUniverseOperationConflict), errors.Is(err, catalog.ErrUniverseMembershipExcluded):
		status = http.StatusConflict
	case errors.Is(err, catalog.ErrUniverseSearchBoundsExceeded), errors.Is(err, catalog.ErrUniverseResolveBoundsExceeded):
		status = http.StatusServiceUnavailable
	}
	message := "Universe operation failed"
	if status == http.StatusNotFound {
		message = "Universe or exact Work decision was not found"
	} else if status == http.StatusConflict {
		message = "Universe decision conflicts with an existing manual decision"
	} else if status == http.StatusServiceUnavailable {
		message = "local Universe catalog is temporarily unavailable for this bounded operation"
	}
	writeUniverseError(w, status, message)
}

func writeUniverseError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
