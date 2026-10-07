package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"lernae/internal/acquisition"
)

// AcquisitionCandidateService keeps discovery and exact-choice rules below HTTP.
// It is separate from P4-01's acceptance/polling interface.
type AcquisitionCandidateService interface {
	Discover(context.Context, string) (acquisition.Discovery, error)
	Select(context.Context, string, string) (acquisition.Selection, error)
	GetSelection(context.Context, string) (acquisition.Selection, error)
}

// Public projections are explicit allowlists. Never serialize a domain Option
// or Selection directly: execution references belong only to trusted execution.
type AcquisitionCandidateResponse struct {
	OperationID     string `json:"operation_id"`
	EditionID       string `json:"edition_id"`
	ProviderID      string `json:"provider_id"`
	CandidateID     string `json:"candidate_id"`
	CandidateHandle string `json:"candidate_handle"`
	Title           string `json:"title"`
	Label           string `json:"label,omitempty"`
	Language        string `json:"language,omitempty"`
}

type AcquisitionProviderFailureResponse struct {
	ProviderID string `json:"provider_id"`
	Category   string `json:"category"`
	Message    string `json:"message"`
}

type AcquisitionCandidatesResponse struct {
	OperationID string                               `json:"operation_id"`
	EditionID   string                               `json:"edition_id"`
	Outcome     string                               `json:"outcome"`
	Candidates  []AcquisitionCandidateResponse       `json:"candidates"`
	Failures    []AcquisitionProviderFailureResponse `json:"failures"`
}

type AcquisitionSelectionResponse struct {
	Candidate  AcquisitionCandidateResponse `json:"candidate"`
	SelectedAt time.Time                    `json:"selected_at"`
}

type AcquisitionSelectionRequest struct {
	CandidateHandle string `json:"candidate_handle"`
}

func (handler StatusHandler) acquisitionCandidates(w http.ResponseWriter, request *http.Request) {
	if !getOnly(w, request) || !candidateQueryAllowed(w, request) {
		return
	}
	if handler.AcquisitionCandidates == nil {
		writeCandidateServiceError(w, acquisition.ErrUnavailable)
		return
	}
	result, err := handler.AcquisitionCandidates.Discover(request.Context(), request.PathValue("id"))
	if err != nil {
		writeCandidateServiceError(w, err)
		return
	}
	response := AcquisitionCandidatesResponse{
		OperationID: result.JobID, EditionID: string(result.EditionID), Outcome: string(result.Outcome),
		Candidates: make([]AcquisitionCandidateResponse, 0, len(result.Candidates)),
		Failures:   make([]AcquisitionProviderFailureResponse, 0, len(result.Failures)),
	}
	for _, candidate := range result.Candidates {
		response.Candidates = append(response.Candidates, publicCandidate(candidate))
	}
	for _, failure := range result.Failures {
		category, message := publicProviderFailure(failure.Category)
		response.Failures = append(response.Failures, AcquisitionProviderFailureResponse{
			ProviderID: failure.ProviderID, Category: category, Message: message,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (handler StatusHandler) acquisitionSelection(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		methodNotAllowed(w, "GET, POST")
		return
	}
	if !candidateQueryAllowed(w, request) {
		return
	}
	if handler.AcquisitionCandidates == nil {
		writeCandidateServiceError(w, acquisition.ErrUnavailable)
		return
	}
	var selection acquisition.Selection
	var err error
	if request.Method == http.MethodGet {
		selection, err = handler.AcquisitionCandidates.GetSelection(request.Context(), request.PathValue("id"))
	} else {
		mediaType, _, parseErr := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if parseErr != nil || mediaType != "application/json" {
			writeAcquisitionError(w, http.StatusBadRequest, "request must be JSON")
			return
		}
		var payload AcquisitionSelectionRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			writeAcquisitionDecodeError(w, err)
			return
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			writeAcquisitionDecodeError(w, err)
			return
		}
		selection, err = handler.AcquisitionCandidates.Select(request.Context(), request.PathValue("id"), payload.CandidateHandle)
	}
	if err != nil {
		writeCandidateServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, AcquisitionSelectionResponse{
		Candidate: publicCandidate(selection.Candidate), SelectedAt: selection.SelectedAt,
	})
}

func candidateQueryAllowed(w http.ResponseWriter, request *http.Request) bool {
	if request.URL.RawQuery != "" {
		writeAcquisitionError(w, http.StatusBadRequest, "acquisition candidate query parameters are not supported")
		return false
	}
	return true
}

func publicCandidate(candidate acquisition.Candidate) AcquisitionCandidateResponse {
	return AcquisitionCandidateResponse{
		OperationID: candidate.JobID, EditionID: string(candidate.EditionID),
		ProviderID: candidate.ProviderID, CandidateID: candidate.Option.ID, CandidateHandle: candidate.Handle,
		Title: candidate.Option.Metadata.Title, Label: candidate.Option.Metadata.Label, Language: candidate.Option.Metadata.Language,
	}
}

func publicProviderFailure(category acquisition.Error) (string, string) {
	switch category {
	case acquisition.ErrTimeout:
		return string(category), "Acquisition provider timed out."
	case acquisition.ErrInvalidResponse:
		return string(category), "Acquisition provider returned invalid candidates."
	default:
		return string(acquisition.ErrProvider), "Acquisition provider is unavailable."
	}
}

func writeCandidateServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acquisition.ErrInvalidID):
		writeAcquisitionError(w, http.StatusBadRequest, "invalid acquisition or candidate identifier")
	case errors.Is(err, acquisition.ErrNotFound):
		writeAcquisitionError(w, http.StatusNotFound, "acquisition operation was not found")
	case errors.Is(err, acquisition.ErrNoSelection):
		writeAcquisitionError(w, http.StatusNotFound, "acquisition selection was not found")
	case errors.Is(err, acquisition.ErrUnknownCandidate):
		writeAcquisitionError(w, http.StatusNotFound, "candidate was not found in the current discovery snapshot")
	case errors.Is(err, acquisition.ErrSnapshotExpired):
		writeAcquisitionError(w, http.StatusConflict, "candidate snapshot is unavailable; discover candidates again")
	case errors.Is(err, acquisition.ErrConflict):
		writeAcquisitionError(w, http.StatusConflict, "acquisition already has a different immutable selection")
	case errors.Is(err, acquisition.ErrNotQueued):
		writeAcquisitionError(w, http.StatusConflict, "acquisition must be queued to discover or select candidates")
	case errors.Is(err, acquisition.ErrCancelled):
		writeAcquisitionError(w, http.StatusRequestTimeout, "acquisition request was cancelled")
	default:
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition candidate service is temporarily unavailable")
	}
}
