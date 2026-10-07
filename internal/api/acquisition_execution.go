package api

import (
	"context"
	"errors"
	"io"
	"net/http"

	"lernae/internal/acquisition"
)

type AcquisitionExecutionService interface {
	Execute(context.Context, string) (acquisition.Reservation, error)
}

type AcquisitionExecutionResponse struct {
	OperationID string `json:"operation_id"`
	ExecutionID string `json:"execution_id"`
	Outcome     string `json:"dispatch_outcome"`
}

// WithAcquisitionExecution composes the manually invoked route without adding
// execution authority to candidate discovery or changing their shared handler.
func WithAcquisitionExecution(next http.Handler, service AcquisitionExecutionService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/acquisitions/{id}/execute", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if request.URL.RawQuery != "" {
			writeAcquisitionError(w, http.StatusBadRequest, "execution query parameters are not supported")
			return
		}
		// This endpoint accepts no candidate, reference, or client payload.
		var one [1]byte
		if n, err := io.ReadFull(request.Body, one[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			writeAcquisitionError(w, http.StatusBadRequest, "execution request must have an empty body")
			return
		}
		if service == nil {
			writeExecutionError(w, acquisition.ErrExecutor)
			return
		}
		result, err := service.Execute(request.Context(), request.PathValue("id"))
		if err != nil {
			writeExecutionError(w, err)
			return
		}
		status := http.StatusAccepted
		switch result.Outcome {
		case acquisition.DispatchAccepted, acquisition.DispatchReserved:
		case acquisition.DispatchRejected, acquisition.DispatchUnconfirmed:
			status = http.StatusConflict
		default:
			writeExecutionError(w, acquisition.ErrUnavailable)
			return
		}
		w.Header().Set("Location", "/api/v1/acquisitions/"+result.JobID)
		writeJSON(w, status, AcquisitionExecutionResponse{
			OperationID: result.JobID, ExecutionID: result.ExecutionID, Outcome: string(result.Outcome),
		})
	})
	mux.Handle("/", next)
	return mux
}

func writeExecutionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acquisition.ErrNoSelection):
		writeAcquisitionError(w, http.StatusConflict, "acquisition requires an accepted selection before execution")
	case errors.Is(err, acquisition.ErrExecutionProvider):
		writeAcquisitionError(w, http.StatusServiceUnavailable, "selected acquisition provider execution capability is unavailable")
	case errors.Is(err, acquisition.ErrExecutor):
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition executor is unavailable")
	case errors.Is(err, acquisition.ErrUnresolved):
		writeAcquisitionError(w, http.StatusConflict, "accepted acquisition selection cannot be resolved for execution")
	case errors.Is(err, acquisition.ErrNotQueued):
		writeAcquisitionError(w, http.StatusConflict, "acquisition must be queued before initial execution")
	case errors.Is(err, acquisition.ErrTimeout):
		writeAcquisitionError(w, http.StatusServiceUnavailable, "acquisition execution resolution timed out")
	default:
		// Existing fixed mappings include invalid IDs, kind isolation, state and
		// cancellation. Unknown/raw errors always collapse to a safe fixed 503.
		writeCandidateServiceError(w, err)
	}
}
