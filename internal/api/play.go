package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"lernae/internal/jobs"
	"lernae/internal/playcoordinator"
	"lernae/internal/providers"
)

const maxPlayRequestBodyBytes = 64 << 10

// PlayStarter accepts user intent and returns once a durable PLAY operation is
// accepted. It deliberately has no request context: accepted work belongs to
// the Server-lifetime coordinator, not the browser connection.
type PlayStarter interface {
	Start(playcoordinator.Intent) (playcoordinator.Accepted, error)
}

// PlayJobReader is the read-only persistence surface needed to safely project
// an operation for polling.
type PlayJobReader interface {
	Get(context.Context, string) (jobs.Job, error)
}

type PlayRequest struct {
	WorkIdentity PlayWorkIdentity `json:"work_identity"`
	Edition      PlayEdition      `json:"edition"`
}

type PlayWorkIdentity struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
}

type PlayEdition struct {
	Platform string `json:"platform"`
	Format   string `json:"format"`
}

type PlayAcceptedResponse struct {
	OperationID string `json:"operation_id"`
}

type PlayRestoreProgress struct {
	CurrentBytes int64 `json:"current_bytes"`
	TotalBytes   int64 `json:"total_bytes"`
}

type PlaySafeError struct {
	Category string `json:"category"`
	Message  string `json:"message"`
}

// PlayOperationResponse is an explicit public projection. Never replace it
// with jobs.Job: the durable model contains internal messages and error detail.
type PlayOperationResponse struct {
	OperationID     string               `json:"operation_id"`
	Status          string               `json:"status"`
	Phase           string               `json:"phase,omitempty"`
	RestoreProgress *PlayRestoreProgress `json:"restore_progress,omitempty"`
	SessionID       string               `json:"session_id,omitempty"`
	Error           *PlaySafeError       `json:"error,omitempty"`
}

func (handler StatusHandler) startPlay(w http.ResponseWriter, request *http.Request) {
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
	var payload PlayRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxPlayRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PLAY request"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PLAY request"})
		return
	}
	if !validPlayRequest(payload) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PLAY request"})
		return
	}
	if handler.Play == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "PLAY is not configured"})
		return
	}
	accepted, err := handler.Play.Start(playcoordinator.Intent{
		WorkIdentity: providers.ExternalWorkIdentity{Provider: payload.WorkIdentity.Provider, ExternalID: payload.WorkIdentity.ExternalID},
		Edition:      playcoordinator.EditionIntent{Platform: payload.Edition.Platform, Format: payload.Edition.Format},
	})
	if err != nil {
		status, message := playAcceptanceError(err)
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	if !validOpaqueID(accepted.JobID) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "PLAY could not be accepted safely"})
		return
	}
	writeJSON(w, http.StatusAccepted, PlayAcceptedResponse{OperationID: accepted.JobID})
}

func (handler StatusHandler) getPlay(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := request.PathValue("id")
	if !validOpaqueID(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "PLAY operation was not found"})
		return
	}
	if handler.PlayJobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "PLAY polling is not configured"})
		return
	}
	job, err := handler.PlayJobs.Get(request.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && (job.ID != id || job.Kind != jobs.KindPlay)) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "PLAY operation was not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "PLAY operation is temporarily unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, safePlayOperation(job))
}

func validPlayRequest(payload PlayRequest) bool {
	return safePlayIdentityValue(payload.WorkIdentity.Provider, 64) &&
		safePlayIdentityValue(payload.WorkIdentity.ExternalID, 128) &&
		payload.Edition.Platform == "gamecube" && payload.Edition.Format == "disc_image"
}

func safePlayIdentityValue(value string, limit int) bool {
	if !nonEmptyBounded(value, limit) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validOpaqueID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && (character == '-' || character == '_')) {
			continue
		}
		return false
	}
	return true
}

func playAcceptanceError(err error) (int, string) {
	switch {
	case errors.Is(err, playcoordinator.ErrInvalidIntent):
		return http.StatusBadRequest, "unsupported PLAY intent"
	case errors.Is(err, playcoordinator.ErrPlayConflict):
		return http.StatusConflict, "another PLAY operation is active"
	case errors.Is(err, playcoordinator.ErrServiceStopping):
		return http.StatusServiceUnavailable, "PLAY service is stopping"
	default:
		return http.StatusServiceUnavailable, "PLAY could not be accepted"
	}
}

func safePlayOperation(job jobs.Job) PlayOperationResponse {
	view := PlayOperationResponse{
		OperationID: job.ID,
		Status:      normalizedPlayStatus(job.Status),
		Phase:       normalizedPlayPhase(job.Phase),
	}
	if job.ProgressTotal > 0 && job.ProgressCurrent >= 0 && job.ProgressCurrent <= job.ProgressTotal {
		view.RestoreProgress = &PlayRestoreProgress{CurrentBytes: job.ProgressCurrent, TotalBytes: job.ProgressTotal}
	}
	if validSessionID(job.SessionID) {
		view.SessionID = job.SessionID
	}
	switch job.Status {
	case jobs.StatusFailed:
		view.Error = safePlayFailure(job.ErrorCode)
	case jobs.StatusInterrupted:
		view.Error = &PlaySafeError{Category: "operation_interrupted", Message: "PLAY ended before normal completion was confirmed."}
	case jobs.StatusCancelled:
		view.Error = &PlaySafeError{Category: "operation_cancelled", Message: "PLAY was cancelled before completion."}
	}
	return view
}

func normalizedPlayStatus(status jobs.Status) string {
	switch status {
	case jobs.StatusQueued, jobs.StatusRunning, jobs.StatusWaitingOnAgent, jobs.StatusSucceeded,
		jobs.StatusFailed, jobs.StatusInterrupted, jobs.StatusCancelled:
		return string(status)
	default:
		return "unknown"
	}
}

func normalizedPlayPhase(phase jobs.Phase) string {
	switch phase {
	case jobs.PhaseRestore, jobs.PhaseLaunch, jobs.PhasePlaying:
		return string(phase)
	default:
		return ""
	}
}

func validSessionID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && (character == '-' || character == '_')) {
			continue
		}
		return false
	}
	return true
}

func safePlayFailure(code string) *PlaySafeError {
	switch code {
	case "inventory_unavailable":
		return &PlaySafeError{Category: "inventory_unavailable", Message: "No exact owned game is available for this Edition."}
	case "inventory_resolution_failed":
		return &PlaySafeError{Category: "inventory_resolution_failed", Message: "Inventory resolution failed. Try again shortly."}
	case "inventory_conflict":
		return &PlaySafeError{Category: "inventory_conflict", Message: "More than one owned match prevents safe playback."}
	case "launch_source_unavailable":
		return &PlaySafeError{Category: "launch_source_unavailable", Message: "No safe launch source is available."}
	case "restore_failed":
		return &PlaySafeError{Category: "restore_failed", Message: "The game could not be restored safely."}
	case "cache_metadata_cleanup_failed":
		return &PlaySafeError{Category: "local_cache_unavailable", Message: "The local game cache could not be updated safely."}
	case "local_cache_invalid":
		return &PlaySafeError{Category: "local_cache_invalid", Message: "The local game copy did not pass validation."}
	case "game_exit_nonzero":
		return &PlaySafeError{Category: "game_exit", Message: "The game did not exit normally."}
	case "session_start_rejected", "session_start_unconfirmed", "session_terminal_unconfirmed":
		return &PlaySafeError{Category: "session_unconfirmed", Message: "PLAY could not confirm the game Session safely."}
	case "play_interrupted", "launch_unconfirmed":
		return &PlaySafeError{Category: "operation_interrupted", Message: "PLAY ended before normal completion was confirmed."}
	default:
		return &PlaySafeError{Category: "operation_failed", Message: "PLAY could not complete safely."}
	}
}
