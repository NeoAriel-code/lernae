package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lernae/internal/jobs"
	"lernae/internal/playcoordinator"
	"lernae/internal/providers"
)

const validPlayRequestBody = `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`

type playStarterStub struct {
	start func(playcoordinator.Intent) (playcoordinator.Accepted, error)
}

func (stub playStarterStub) Start(intent playcoordinator.Intent) (playcoordinator.Accepted, error) {
	return stub.start(intent)
}

type playJobReaderStub struct {
	job jobs.Job
	err error
	id  string
}

func (stub playJobReaderStub) Get(_ context.Context, id string) (jobs.Job, error) {
	stub.id = id
	return stub.job, stub.err
}

func TestPlayEndpointAcceptsOnlyIdentityIntentAndReturnsOpaqueOperation(t *testing.T) {
	var got playcoordinator.Intent
	startCalls := 0
	request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(validPlayRequestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler := (StatusHandler{Play: playStarterStub{start: func(intent playcoordinator.Intent) (playcoordinator.Accepted, error) {
		startCalls++
		got = intent
		return playcoordinator.Accepted{JobID: "opaque-operation-id"}, nil
	}}}).Routes()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status/body = %d/%q, want 202", response.Code, response.Body.String())
	}
	var body struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	want := playcoordinator.Intent{
		WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"},
		Edition:      playcoordinator.EditionIntent{Platform: "gamecube", Format: "disc_image"},
	}
	if startCalls != 1 || got != want {
		t.Fatalf("Start calls/intent = %d/%#v, want once with %#v", startCalls, got, want)
	}
	if body.OperationID != "opaque-operation-id" {
		t.Fatalf("operation_id = %q, want opaque accepted ID", body.OperationID)
	}
	if strings.Contains(response.Body.String(), "status") || strings.Contains(response.Body.String(), "gamecube") {
		t.Fatalf("acceptance response included non-reference details: %s", response.Body.String())
	}
}

func TestPlayEndpointRejectsAdditionalAuthorityFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level asset authority",
			body: `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"},"asset_id":"asset-secret"}`,
		},
		{
			name: "nested work identity locator",
			body: `{"work_identity":{"provider":"igdb","external_id":"1565","path":"/private/game.iso"},"edition":{"platform":"gamecube","format":"disc_image"}}`,
		},
		{
			name: "nested edition launch flags",
			body: `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image","flags":["--unsafe"]}}`,
		},
		{
			name: "top-level process authority",
			body: `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"},"environment":{"TOKEN":"secret"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			(StatusHandler{Play: playStarterStub{start: func(playcoordinator.Intent) (playcoordinator.Accepted, error) {
				called = true
				return playcoordinator.Accepted{JobID: "should-not-start"}, nil
			}}}).Routes().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || called {
				t.Fatalf("status/called = %d/%t, want 400 without coordinator call; body=%q", response.Code, called, response.Body.String())
			}
		})
	}
}

func TestPlayEndpointReturnsBeforeAcceptedOperationCompletes(t *testing.T) {
	operationDone := make(chan struct{})
	finishOperation := make(chan struct{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(validPlayRequestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	starter := playStarterStub{start: func(playcoordinator.Intent) (playcoordinator.Accepted, error) {
		go func() {
			<-finishOperation
			close(operationDone)
		}()
		return playcoordinator.Accepted{JobID: "accepted-while-active"}, nil
	}}
	(StatusHandler{Play: starter}.Routes()).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want prompt 202 while operation is active", response.Code)
	}
	select {
	case <-operationDone:
		t.Fatal("operation completed before the test released its simulated PLAY lifetime")
	default:
	}
	close(finishOperation)
	<-operationDone
}

func TestPlayEndpointAcceptedWorkSurvivesRequestCancellation(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	operationFinished := make(chan struct{})
	finishOperation := make(chan struct{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(validPlayRequestBody)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	starter := playStarterStub{start: func(playcoordinator.Intent) (playcoordinator.Accepted, error) {
		go func() {
			<-finishOperation
			close(operationFinished)
		}()
		return playcoordinator.Accepted{JobID: "accepted-operation"}, nil
	}}
	(StatusHandler{Play: starter}.Routes()).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	cancelRequest()
	close(finishOperation)
	select {
	case <-operationFinished:
	case <-requestCtx.Done():
		// This is expected to be signaled, but must not own the operation.
		<-operationFinished
	}
}

func TestPlayPollingReturnsOnlyAllowlistedSafeFields(t *testing.T) {
	const privateError = "secret-error-detail:/private/root game.iso rclone copy --token super-secret raw-manifest"
	job := jobs.Job{
		ID: "operation-123", Kind: jobs.KindPlay, Status: jobs.StatusFailed, Phase: jobs.PhaseRestore,
		SessionID: "session-456", ProgressCurrent: 13, ProgressTotal: 29,
		Message: "private-message sentinel", ErrorCode: "private-error-code sentinel", ErrorDetail: privateError,
	}
	reader := playJobReaderStub{job: job}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/play/operation-123", nil)
	response := httptest.NewRecorder()
	(StatusHandler{PlayJobs: reader}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%q, want 200 safe operation view", response.Code, response.Body.String())
	}
	for _, secret := range []string{"private-message sentinel", "private-error-code sentinel", "secret-error-detail", "/private/root", "game.iso", "rclone copy", "super-secret", "raw-manifest"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("poll response leaked %q: %s", secret, response.Body.String())
		}
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []string{"operation_id", "status", "phase", "restore_progress", "session_id", "error"} {
		if _, exists := body[allowed]; !exists {
			t.Fatalf("allowlisted field %q missing from %s", allowed, response.Body.String())
		}
	}
	for _, forbidden := range []string{"id", "kind", "message", "error_code", "error_detail", "created_at", "updated_at", "progress_current", "progress_total"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("non-allowlisted Job field %q present in %s", forbidden, response.Body.String())
		}
	}
	var view PlayOperationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.OperationID != job.ID || view.Status != "failed" || view.Phase != "restore" ||
		view.RestoreProgress == nil || view.RestoreProgress.CurrentBytes != 13 || view.RestoreProgress.TotalBytes != 29 ||
		view.SessionID != job.SessionID || view.Error == nil || view.Error.Category != "operation_failed" {
		t.Fatalf("safe operation view = %#v, want truthful allowlisted values", view)
	}
}

func TestPlayPollingMapsUnknownOperationToSafeNotFound(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/play/unknown-id", nil)
	response := httptest.NewRecorder()
	(StatusHandler{PlayJobs: playJobReaderStub{err: jobs.ErrNotFound}}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || response.Body.String() != "{\"error\":\"PLAY operation was not found\"}\n" {
		t.Fatalf("unknown operation response = %d/%q, want safe 404", response.Code, response.Body.String())
	}
}

func TestPlayPollingPreservesTruthfulTerminalStatuses(t *testing.T) {
	tests := []struct {
		name         string
		status       jobs.Status
		phase        jobs.Phase
		currentBytes int64
		totalBytes   int64
		wantProgress bool
		want         string
		wantErr      string
	}{
		{name: "normal completion", status: jobs.StatusSucceeded, phase: jobs.PhasePlaying, currentBytes: 13, totalBytes: 13, wantProgress: true, want: "succeeded"},
		{name: "failed completion", status: jobs.StatusFailed, want: "failed", wantErr: "operation_failed"},
		{name: "interrupted completion", status: jobs.StatusInterrupted, want: "interrupted", wantErr: "operation_interrupted"},
		{name: "cancelled before work", status: jobs.StatusCancelled, want: "cancelled", wantErr: "operation_cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := jobs.Job{
				ID: "terminal-operation", Kind: jobs.KindPlay, Status: test.status, Phase: test.phase,
				ProgressCurrent: test.currentBytes, ProgressTotal: test.totalBytes,
				ErrorCode: "untrusted code", ErrorDetail: "untrusted details",
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/play/terminal-operation", nil)
			response := httptest.NewRecorder()
			(StatusHandler{PlayJobs: playJobReaderStub{job: job}}).Routes().ServeHTTP(response, request)
			var view PlayOperationResponse
			if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || view.Status != test.want {
				t.Fatalf("status/view = %d/%#v, want 200/%q", response.Code, view, test.want)
			}
			if test.wantErr == "" {
				if view.Error != nil {
					t.Fatalf("successful operation error = %#v, want none", view.Error)
				}
			} else if view.Error == nil || view.Error.Category != test.wantErr {
				t.Fatalf("terminal error = %#v, want category %q", view.Error, test.wantErr)
			}
			if (view.RestoreProgress != nil) != test.wantProgress {
				t.Fatalf("restore progress = %#v, want present=%t", view.RestoreProgress, test.wantProgress)
			}
		})
	}
}

func TestPlayPollingDistinguishesUnavailableInventoryFromResolutionFailure(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		wantCategory string
		wantMessage  string
	}{
		{
			name: "no exact owned Asset",
			code: "inventory_unavailable", wantCategory: "inventory_unavailable",
			wantMessage: "No exact owned game is available for this Edition.",
		},
		{
			name: "technical catalog error",
			code: "inventory_resolution_failed", wantCategory: "inventory_resolution_failed",
			wantMessage: "Inventory resolution failed. Try again shortly.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := jobs.Job{
				ID: "inventory-operation", Kind: jobs.KindPlay, Status: jobs.StatusFailed,
				ErrorCode: test.code, ErrorDetail: "private database path and SQL error sentinel",
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/play/inventory-operation", nil)
			response := httptest.NewRecorder()
			(StatusHandler{PlayJobs: playJobReaderStub{job: job}}).Routes().ServeHTTP(response, request)
			var view PlayOperationResponse
			if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || view.Error == nil || view.Error.Category != test.wantCategory || view.Error.Message != test.wantMessage {
				t.Fatalf("inventory failure view = %d/%#v, want safe %q/%q", response.Code, view.Error, test.wantCategory, test.wantMessage)
			}
			if strings.Contains(response.Body.String(), "private database path") || strings.Contains(response.Body.String(), "SQL error sentinel") {
				t.Fatalf("poll response exposed technical failure detail: %s", response.Body.String())
			}
		})
	}
}

func TestPlayEndpointReturnsSafeValidationAndServiceErrors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		content    string
		startErr   error
		wantStatus int
		wantText   string
	}{
		{name: "malformed JSON", body: `{"work_identity":`, content: "application/json", wantStatus: http.StatusBadRequest},
		{name: "unsupported edition", body: `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"playstation-2","format":"disc_image"}}`, content: "application/json", wantStatus: http.StatusBadRequest},
		{name: "missing content type", body: validPlayRequestBody, wantStatus: http.StatusBadRequest},
		{name: "coordinator rejects intent", body: validPlayRequestBody, content: "application/json", startErr: playcoordinator.ErrInvalidIntent, wantStatus: http.StatusBadRequest},
		{name: "active operation conflict", body: validPlayRequestBody, content: "application/json", startErr: playcoordinator.ErrPlayConflict, wantStatus: http.StatusConflict},
		{name: "service stopping", body: validPlayRequestBody, content: "application/json", startErr: playcoordinator.ErrServiceStopping, wantStatus: http.StatusServiceUnavailable},
		{name: "internal acceptance error", body: validPlayRequestBody, content: "application/json", startErr: errors.New("private db path and SQL details"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(test.body))
			if test.content != "" {
				request.Header.Set("Content-Type", test.content)
			}
			response := httptest.NewRecorder()
			handler := StatusHandler{Play: playStarterStub{start: func(playcoordinator.Intent) (playcoordinator.Accepted, error) {
				called = true
				return playcoordinator.Accepted{}, test.startErr
			}}}
			handler.Routes().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status/body = %d/%q, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if test.startErr == nil && called {
				t.Fatal("invalid input reached coordinator")
			}
			if strings.Contains(response.Body.String(), "private db path") || strings.Contains(response.Body.String(), "SQL details") {
				t.Fatalf("acceptance error leaked internal detail: %s", response.Body.String())
			}
			if test.wantText != "" && !strings.Contains(response.Body.String(), test.wantText) {
				t.Fatalf("response %q does not contain safe text %q", response.Body.String(), test.wantText)
			}
		})
	}
}
