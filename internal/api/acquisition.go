package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"lernae/internal/jobs"
)

// AcquisitionService accepts durable intent and exposes only kind-scoped reads.
// It has no provider selection, worker, or Agent execution authority.
type AcquisitionService interface {
	CreateAcquisition(context.Context, string) (jobs.Job, error)
	GetAcquisition(context.Context, string) (jobs.Job, error)
	ListAcquisitions(context.Context, int) ([]jobs.Job, error)
}

type AcquisitionRequest struct {
	EditionID string `json:"edition_id"`
}

type AcquisitionAcceptedResponse struct {
	OperationID string `json:"operation_id"`
}

type AcquisitionSafeError struct {
	Category string `json:"category"`
	Message  string `json:"message"`
}

// AcquisitionOperationResponse deliberately excludes internal Job messages and
// error detail. Public terminal reasons are fixed projections, never raw errors.
type AcquisitionOperationResponse struct {
	OperationID string                `json:"operation_id"`
	EditionID   string                `json:"edition_id"`
	Status      string                `json:"status"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
	Error       *AcquisitionSafeError `json:"error,omitempty"`
}

func (handler StatusHandler) acquisitionCollection(w http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodPost:
		handler.createAcquisition(w, request)
	case http.MethodGet:
		handler.listAcquisitions(w, request)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (handler StatusHandler) createAcquisition(w http.ResponseWriter, request *http.Request) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request must be JSON"})
		return
	}
	var payload AcquisitionRequest
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
	if handler.Acquisitions == nil {
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition service is unavailable")
		return
	}
	job, err := handler.Acquisitions.CreateAcquisition(request.Context(), payload.EditionID)
	if err != nil {
		writeAcquisitionServiceError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/acquisitions/"+job.ID)
	writeJSON(w, http.StatusAccepted, AcquisitionAcceptedResponse{OperationID: job.ID})
}

func (handler StatusHandler) getAcquisition(w http.ResponseWriter, request *http.Request) {
	if !getOnly(w, request) {
		return
	}
	if handler.Acquisitions == nil {
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition service is unavailable")
		return
	}
	id := request.PathValue("id")
	job, err := handler.Acquisitions.GetAcquisition(request.Context(), id)
	if err != nil {
		writeAcquisitionServiceError(w, err)
		return
	}
	if job.Kind != jobs.KindAcquire || job.ID != id {
		writeAcquisitionServiceError(w, jobs.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, safeAcquisitionOperation(job))
}

func (handler StatusHandler) listAcquisitions(w http.ResponseWriter, request *http.Request) {
	limit := jobs.DefaultAcquisitionLimit
	for key, values := range request.URL.Query() {
		if key != "limit" || len(values) != 1 {
			writeAcquisitionServiceError(w, jobs.ErrInvalidAcquisitionLimit)
			return
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil {
			writeAcquisitionServiceError(w, jobs.ErrInvalidAcquisitionLimit)
			return
		}
		limit = parsed
	}
	if handler.Acquisitions == nil {
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition service is unavailable")
		return
	}
	list, err := handler.Acquisitions.ListAcquisitions(request.Context(), limit)
	if err != nil {
		writeAcquisitionServiceError(w, err)
		return
	}
	result := make([]AcquisitionOperationResponse, 0, len(list))
	for _, job := range list {
		result = append(result, safeAcquisitionOperation(job))
	}
	writeJSON(w, http.StatusOK, result)
}

func writeAcquisitionDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAcquisitionError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	writeAcquisitionError(w, http.StatusBadRequest, "invalid acquisition request")
}

func writeAcquisitionServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrInvalidAcquisitionTarget):
		writeAcquisitionError(w, http.StatusBadRequest, "invalid acquisition Edition target")
	case errors.Is(err, jobs.ErrEditionNotFound):
		writeAcquisitionError(w, http.StatusNotFound, "Edition was not found")
	case errors.Is(err, jobs.ErrNotFound):
		writeAcquisitionError(w, http.StatusNotFound, "acquisition operation was not found")
	case errors.Is(err, jobs.ErrInvalidAcquisitionLimit):
		writeAcquisitionError(w, http.StatusBadRequest, "invalid acquisition list limit")
	default:
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition operation is temporarily unavailable")
	}
}

func writeAcquisitionError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func safeAcquisitionOperation(job jobs.Job) AcquisitionOperationResponse {
	view := AcquisitionOperationResponse{
		OperationID: job.ID, EditionID: job.TargetEditionID, Status: string(job.Status),
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
	switch job.Status {
	case jobs.StatusQueued, jobs.StatusRunning, jobs.StatusSucceeded:
	case jobs.StatusFailed:
		view.Error = &AcquisitionSafeError{Category: "acquisition_failed", Message: "Acquisition could not complete safely."}
	case jobs.StatusInterrupted:
		view.Error = &AcquisitionSafeError{Category: "acquisition_interrupted", Message: "Acquisition ended before completion was confirmed."}
	case jobs.StatusCancelled:
		view.Error = &AcquisitionSafeError{Category: "acquisition_cancelled", Message: "Acquisition was cancelled before execution."}
	default:
		view.Status = "unknown"
	}
	return view
}
