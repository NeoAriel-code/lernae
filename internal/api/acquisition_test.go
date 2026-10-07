package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"lernae/internal/database"
	"lernae/internal/jobs"
)

func acquisitionAPI(t *testing.T) (http.Handler, *jobs.Service) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	service := jobs.NewService(jobs.NewSQLiteRepository(db))
	return (StatusHandler{Acquisitions: service}).Routes(), service
}

func acquisitionRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAcquisitionAPICreatePollListAndIsolation(t *testing.T) {
	handler, service := acquisitionAPI(t)
	response := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions", `{"edition_id":"edition-1"}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create = %d/%s", response.Code, response.Body.String())
	}
	var accepted struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || !validOpaqueID(accepted.OperationID) {
		t.Fatalf("accepted = %#v, %v", accepted, err)
	}
	if response.Header().Get("Location") != "/api/v1/acquisitions/"+accepted.OperationID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	duplicate := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions", `{"edition_id":"edition-1"}`)
	if duplicate.Code != http.StatusAccepted || duplicate.Body.String() != response.Body.String() {
		t.Fatalf("duplicate = %d/%s", duplicate.Code, duplicate.Body.String())
	}
	polled := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions/"+accepted.OperationID, "")
	if polled.Code != http.StatusOK || polled.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("poll = %d/%s", polled.Code, polled.Body.String())
	}
	var operation AcquisitionOperationResponse
	if err := json.Unmarshal(polled.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.OperationID != accepted.OperationID || operation.EditionID != "edition-1" || operation.Status != "queued" || operation.CreatedAt.IsZero() || operation.UpdatedAt.IsZero() {
		t.Fatalf("poll body = %#v", operation)
	}
	listed := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions?limit=1", "")
	var list []AcquisitionOperationResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil || listed.Code != http.StatusOK || len(list) != 1 || list[0] != operation {
		t.Fatalf("list = %d/%s, %v", listed.Code, listed.Body.String(), err)
	}
	play, err := service.Create(context.Background(), jobs.KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{play.ID, "missing", "bad:id"} {
		if got := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions/"+id, ""); got.Code != http.StatusNotFound {
			t.Fatalf("cross-kind/missing poll = %d/%s", got.Code, got.Body.String())
		}
	}
	playView := (StatusHandler{PlayJobs: service}).Routes()
	if got := acquisitionRequest(playView, http.MethodGet, "/api/v1/play/"+accepted.OperationID, ""); got.Code != http.StatusNotFound {
		t.Fatalf("acquisition leaked into PLAY: %d/%s", got.Code, got.Body.String())
	}
}

func TestAcquisitionAPIRejectsInvalidInput(t *testing.T) {
	handler, _ := acquisitionAPI(t)
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"missing target", `{}`, 400},
		{"null", `null`, 400},
		{"empty", ``, 400},
		{"malformed", `{"edition_id":`, 400},
		{"array", `[]`, 400},
		{"wrong type", `{"edition_id":1}`, 400},
		{"unknown field", `{"edition_id":"edition-1","provider":"fake"}`, 400},
		{"work authority", `{"edition_id":"edition-1","work_id":"work-1"}`, 400},
		{"multiple values", `{"edition_id":"edition-1"} {}`, 400},
		{"padded target", `{"edition_id":" edition-1"}`, 400},
		{"invalid target", `{"edition_id":"../edition-1"}`, 400},
		{"unknown target", `{"edition_id":"missing"}`, 404},
		{"too large", `{"edition_id":"` + strings.Repeat("x", 5000) + `"}`, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions", test.body)
			if response.Code != test.status {
				t.Fatalf("invalid input = %d/%s, want %d", response.Code, response.Body.String(), test.status)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions", strings.NewReader(`{"edition_id":"edition-1"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-JSON request = %d", response.Code)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=x", "limit=", "limit=1&limit=2", "provider=fake"} {
		response := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions?"+query, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %s = %d/%s", query, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/acquisitions", "/api/v1/acquisitions/job-1"} {
		response := acquisitionRequest(handler, http.MethodPatch, path, `{}`)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") == "" {
			t.Fatalf("method rejection = %d/%s", response.Code, response.Body.String())
		}
	}
	response = acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions", "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("invalid requests left jobs: %d/%s", response.Code, response.Body.String())
	}
}

func TestAcquisitionAPIRecentDefaultLimitAndOrder(t *testing.T) {
	handler, service := acquisitionAPI(t)
	ctx := context.Background()
	var created []jobs.Job
	for i := 0; i < 23; i++ {
		job, err := service.CreateAcquisition(ctx, "edition-1")
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, job)
		if err := service.Transition(ctx, job.ID, jobs.StatusCancelled); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(created, func(i, j int) bool {
		if created[i].CreatedAt.Equal(created[j].CreatedAt) {
			return created[i].ID > created[j].ID
		}
		return created[i].CreatedAt.After(created[j].CreatedAt)
	})
	for _, test := range []struct {
		name, query string
		count       int
	}{
		{"default", "", 20},
		{"bounded", "?limit=2", 2},
		{"maximum", "?limit=100", 23},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions"+test.query, "")
			var list []AcquisitionOperationResponse
			if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || response.Code != http.StatusOK || len(list) != test.count {
				t.Fatalf("recent list = %d/%s, %v", response.Code, response.Body.String(), err)
			}
			for i, operation := range list {
				if operation.OperationID != created[i].ID || operation.Status != "cancelled" {
					t.Fatalf("recent[%d] = %#v, want newest first", i, operation)
				}
			}
		})
	}
}

type acquisitionServiceStub struct {
	job jobs.Job
	err error
}

func (s acquisitionServiceStub) CreateAcquisition(context.Context, string) (jobs.Job, error) {
	return s.job, s.err
}

func (s acquisitionServiceStub) GetAcquisition(context.Context, string) (jobs.Job, error) {
	return s.job, s.err
}

func (s acquisitionServiceStub) ListAcquisitions(context.Context, int) ([]jobs.Job, error) {
	return []jobs.Job{s.job}, s.err
}

func TestAcquisitionAPISafeErrorsAndProjection(t *testing.T) {
	for _, path := range []string{"/api/v1/acquisitions", "/api/v1/acquisitions/job-1"} {
		for _, handler := range []http.Handler{
			(StatusHandler{}).Routes(),
			(StatusHandler{Acquisitions: acquisitionServiceStub{err: errors.New("private database path /private/file")}}).Routes(),
		} {
			response := acquisitionRequest(handler, http.MethodGet, path, "")
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("unsafe infrastructure error = %d/%s", response.Code, response.Body.String())
			}
		}
	}
	response := acquisitionRequest((StatusHandler{Acquisitions: acquisitionServiceStub{err: errors.New("private infrastructure")}}).Routes(),
		http.MethodPost, "/api/v1/acquisitions", `{"edition_id":"edition-1"}`)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private") {
		t.Fatalf("unsafe create error = %d/%s", response.Code, response.Body.String())
	}
	for _, status := range []jobs.Status{jobs.StatusFailed, jobs.StatusInterrupted, jobs.StatusCancelled} {
		job := jobs.Job{ID: "job-1", Kind: jobs.KindAcquire, TargetEditionID: "edition-1", Status: status,
			Message: "private raw message", ErrorCode: "private raw code", ErrorDetail: "private raw detail"}
		handler := (StatusHandler{Acquisitions: acquisitionServiceStub{job: job}}).Routes()
		for _, path := range []string{"/api/v1/acquisitions", "/api/v1/acquisitions/job-1"} {
			response := acquisitionRequest(handler, http.MethodGet, path, "")
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "private") || !strings.Contains(response.Body.String(), "acquisition_"+string(status)) {
				t.Fatalf("unsafe terminal projection = %d/%s", response.Code, response.Body.String())
			}
		}
	}
}
