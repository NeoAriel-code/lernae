package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/acquisition"
	"lernae/internal/database"
	"lernae/internal/jobs"
)

func TestAcquisitionCandidateRoutesWithoutServiceAreSafe(t *testing.T) {
	handler := (StatusHandler{}).Routes()
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/acquisitions/job-1/candidates", ""},
		{http.MethodGet, "/api/v1/acquisitions/job-1/selection", ""},
		{http.MethodPost, "/api/v1/acquisitions/job-1/selection", `{"candidate_handle":"` + strings.Repeat("a", 64) + `"}`},
	} {
		response := acquisitionRequest(handler, test.method, test.path, test.body)
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing selection service = %d/%s", response.Code, response.Body.String())
		}
	}
}

type candidateTestProvider struct {
	id       string
	discover func(context.Context, acquisition.Target) ([]acquisition.Option, error)
}

func (p candidateTestProvider) ID() string                       { return p.id }
func (p candidateTestProvider) Eligible(acquisition.Target) bool { return true }
func (p candidateTestProvider) Discover(ctx context.Context, target acquisition.Target) ([]acquisition.Option, error) {
	return p.discover(ctx, target)
}

func candidatesAPI(t *testing.T, providers ...acquisition.Provider) (http.Handler, *acquisition.Service, *jobs.Service, jobs.Job) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "candidates.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	job, err := jobService.CreateAcquisition(context.Background(), "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := acquisition.NewRegistry(providers...)
	if err != nil {
		t.Fatal(err)
	}
	service := acquisition.NewService(jobService, acquisition.NewSQLiteRepository(db), registry)
	return (StatusHandler{Acquisitions: jobService, AcquisitionCandidates: service}).Routes(), service, jobService, job
}

func privateOptionsProvider() candidateTestProvider {
	return candidateTestProvider{id: "source", discover: func(context.Context, acquisition.Target) ([]acquisition.Option, error) {
		return []acquisition.Option{
			{ID: "first", Metadata: acquisition.Metadata{Title: "Readable title", Label: "Standard", Language: "en"}, ExecutionRef: "PRIVATE_EXECUTION_SENTINEL"},
			{ID: "second", Metadata: acquisition.Metadata{Title: "Alternate edition"}, ExecutionRef: "OTHER_PRIVATE_SENTINEL"},
		}, nil
	}}
}

func assertPrivateCandidateData(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	for _, forbidden := range []string{
		"PRIVATE_EXECUTION_SENTINEL", "OTHER_PRIVATE_SENTINEL", "RAW_ERROR_SECRET_SENTINEL",
		"URL_SECRET_SENTINEL", "execution_ref", "ExecutionRef", "raw_error", "payload", "credential",
	} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("private data %q leaked: %s", forbidden, response.Body.String())
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("candidate responses must not be cached")
	}
}

func TestCandidateAPIDiscoverSelectReadAndPrivateAllowlist(t *testing.T) {
	rawFailure := candidateTestProvider{id: "failure", discover: func(context.Context, acquisition.Target) ([]acquisition.Option, error) {
		return nil, errors.New("RAW_ERROR_SECRET_SENTINEL")
	}}
	invalid := candidateTestProvider{id: "invalid", discover: func(context.Context, acquisition.Target) ([]acquisition.Option, error) {
		return []acquisition.Option{{ID: "candidate", Metadata: acquisition.Metadata{Title: "https://private/URL_SECRET_SENTINEL"}, ExecutionRef: "PRIVATE_EXECUTION_SENTINEL"}}, nil
	}}
	handler, service, jobService, job := candidatesAPI(t, privateOptionsProvider(), rawFailure, invalid)
	path := "/api/v1/acquisitions/" + job.ID
	missing := acquisitionRequest(handler, http.MethodGet, path+"/selection", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing selection = %d/%s", missing.Code, missing.Body.String())
	}
	response := acquisitionRequest(handler, http.MethodGet, path+"/candidates", "")
	assertPrivateCandidateData(t, response)
	var discovery AcquisitionCandidatesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &discovery); err != nil || response.Code != http.StatusOK {
		t.Fatalf("discovery = %d/%s, %v", response.Code, response.Body.String(), err)
	}
	if discovery.Outcome != "partial_failure" || len(discovery.Candidates) != 2 || len(discovery.Failures) != 2 ||
		discovery.OperationID != job.ID || discovery.EditionID != "edition-1" {
		t.Fatalf("discovery body = %#v", discovery)
	}
	candidate := discovery.Candidates[0]
	if candidate.CandidateID != "first" || candidate.ProviderID != "source" || candidate.Title != "Readable title" || len(candidate.CandidateHandle) != 64 {
		t.Fatalf("candidate provenance = %#v", candidate)
	}
	body := `{"candidate_handle":"` + candidate.CandidateHandle + `"}`
	selected := acquisitionRequest(handler, http.MethodPost, path+"/selection", body)
	assertPrivateCandidateData(t, selected)
	var selection AcquisitionSelectionResponse
	if err := json.Unmarshal(selected.Body.Bytes(), &selection); err != nil || selected.Code != http.StatusOK || selection.Candidate != candidate || selection.SelectedAt.IsZero() {
		t.Fatalf("selection = %d/%s, %v", selected.Code, selected.Body.String(), err)
	}
	for _, response := range []*httptest.ResponseRecorder{
		acquisitionRequest(handler, http.MethodPost, path+"/selection", body),
		acquisitionRequest(handler, http.MethodGet, path+"/selection", ""),
	} {
		assertPrivateCandidateData(t, response)
		if response.Code != http.StatusOK || response.Body.String() != selected.Body.String() {
			t.Fatalf("immutable retry/read = %d/%s", response.Code, response.Body.String())
		}
	}
	private, err := service.GetSelection(context.Background(), job.ID)
	if err != nil || private.Candidate.Option.ExecutionRef != "PRIVATE_EXECUTION_SENTINEL" {
		t.Fatalf("private exact reference was not retained: %#v, %v", private, err)
	}
	conflict := acquisitionRequest(handler, http.MethodPost, path+"/selection", `{"candidate_handle":"`+discovery.Candidates[1].CandidateHandle+`"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different selection = %d/%s", conflict.Code, conflict.Body.String())
	}
	assertPrivateCandidateData(t, conflict)
	storedJob, err := jobService.GetAcquisition(context.Background(), job.ID)
	if err != nil || storedJob.Status != jobs.StatusQueued {
		t.Fatalf("REST selection executed Job: %#v, %v", storedJob, err)
	}
}

func TestCandidateAPIEmptyProvidersAndInputErrors(t *testing.T) {
	handler, _, jobService, job := candidatesAPI(t)
	path := "/api/v1/acquisitions/" + job.ID
	response := acquisitionRequest(handler, http.MethodGet, path+"/candidates", "")
	var discovery AcquisitionCandidatesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &discovery); err != nil || response.Code != http.StatusOK ||
		discovery.Outcome != "no_providers_configured" || discovery.Candidates == nil || len(discovery.Candidates) != 0 || discovery.Failures == nil {
		t.Fatalf("unconfigured discovery = %d/%s, %v", response.Code, response.Body.String(), err)
	}
	for _, test := range []struct {
		name   string
		method string
		suffix string
		body   string
		status int
	}{
		{"unknown candidate", http.MethodPost, "/selection", `{"candidate_handle":"` + strings.Repeat("a", 64) + `"}`, 404},
		{"missing handle", http.MethodPost, "/selection", `{}`, 400},
		{"null", http.MethodPost, "/selection", `null`, 400},
		{"invalid handle", http.MethodPost, "/selection", `{"candidate_handle":"https://private"}`, 400},
		{"unknown field", http.MethodPost, "/selection", `{"candidate_handle":"x","provider_id":"source"}`, 400},
		{"extra JSON", http.MethodPost, "/selection", `{} {}`, 400},
		{"wrong type", http.MethodPost, "/selection", `{"candidate_handle":1}`, 400},
		{"oversized", http.MethodPost, "/selection", `{"candidate_handle":"` + strings.Repeat("x", 5000) + `"}`, 413},
		{"discovery method", http.MethodPost, "/candidates", `{}`, 405},
		{"selection method", http.MethodPut, "/selection", `{}`, 405},
		{"discovery query", http.MethodGet, "/candidates?provider=source", "", 400},
		{"selection query", http.MethodGet, "/selection?candidate=x", "", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := acquisitionRequest(handler, test.method, path+test.suffix, test.body)
			if response.Code != test.status {
				t.Fatalf("input = %d/%s, want %d", response.Code, response.Body.String(), test.status)
			}
			if test.status == 405 && response.Header().Get("Allow") == "" {
				t.Fatal("method rejection lacks Allow")
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, path+"/selection", strings.NewReader(`{}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing content type = %d", response.Code)
	}
	for _, id := range []string{"unknown", "bad:id"} {
		want := http.StatusNotFound
		if id == "bad:id" {
			want = http.StatusBadRequest
		}
		response := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions/"+id+"/candidates", "")
		if response.Code != want {
			t.Fatalf("identifier %q = %d/%s", id, response.Code, response.Body.String())
		}
	}
	play, err := jobService.Create(context.Background(), jobs.KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if response := acquisitionRequest(handler, http.MethodGet, "/api/v1/acquisitions/"+play.ID+"/selection", ""); response.Code != 404 {
		t.Fatalf("wrong-kind selection = %d", response.Code)
	}
	if err := jobService.Transition(context.Background(), job.ID, jobs.StatusRunning); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/candidates", "/selection"} {
		method, body := http.MethodGet, ""
		if suffix == "/selection" {
			method, body = http.MethodPost, `{"candidate_handle":"`+strings.Repeat("a", 64)+`"}`
		}
		response := acquisitionRequest(handler, method, path+suffix, body)
		if response.Code != http.StatusConflict {
			t.Fatalf("running operation = %d/%s", response.Code, response.Body.String())
		}
	}
}

func TestCandidateAPIRediscoveryInvalidatesOldSnapshot(t *testing.T) {
	p := privateOptionsProvider()
	handler, _, _, job := candidatesAPI(t, p)
	path := "/api/v1/acquisitions/" + job.ID
	response := acquisitionRequest(handler, http.MethodGet, path+"/candidates", "")
	var first AcquisitionCandidatesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	response = acquisitionRequest(handler, http.MethodGet, path+"/candidates", "")
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	response = acquisitionRequest(handler, http.MethodPost, path+"/selection", `{"candidate_handle":"`+first.Candidates[0].CandidateHandle+`"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("stale snapshot was silently rediscovered: %d/%s", response.Code, response.Body.String())
	}
}

func TestCandidateAPIHonorsCallerDeadline(t *testing.T) {
	for _, mode := range []string{"deadline", "before-entry", "active-cancellation"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			finished := make(chan struct{})
			p := candidateTestProvider{id: "slow", discover: func(ctx context.Context, _ acquisition.Target) ([]acquisition.Option, error) {
				close(started)
				defer close(finished)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			handler, _, _, job := candidatesAPI(t, p)
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			defer cancel()
			if mode == "before-entry" {
				cancel()
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/acquisitions/"+job.ID+"/candidates", nil).WithContext(ctx)
			response := httptest.NewRecorder()
			if mode == "active-cancellation" {
				returned := make(chan struct{})
				go func() {
					handler.ServeHTTP(response, request)
					close(returned)
				}()
				// Cancellation starts only AFTER real provider entry, regardless
				// of how long the preceding repository reads took under race.
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					cancel()
					<-returned
					t.Fatal("provider did not enter for active cancellation")
				}
				cancel()
				<-returned
			} else {
				handler.ServeHTTP(response, request)
			}
			if response.Code != http.StatusRequestTimeout {
				t.Fatalf("caller cancellation = %d/%s", response.Code, response.Body.String())
			}
			assertPrivateCandidateData(t, response)
			select {
			case <-started:
				if mode == "before-entry" {
					t.Fatal("already canceled request entered provider")
				}
				select {
				case <-finished:
				default:
					t.Fatal("handler left started provider work running")
				}
			default:
				// The short deadline can expire in SQL Job/Target reads.
				// No provider work exists to join in that valid timeout case.
				if mode == "active-cancellation" {
					t.Fatal("active cancellation did not exercise provider")
				}
			}
		})
	}
}

type failingCandidateService struct{ err error }

func (s failingCandidateService) Discover(context.Context, string) (acquisition.Discovery, error) {
	return acquisition.Discovery{}, s.err
}
func (s failingCandidateService) Select(context.Context, string, string) (acquisition.Selection, error) {
	return acquisition.Selection{}, s.err
}
func (s failingCandidateService) GetSelection(context.Context, string) (acquisition.Selection, error) {
	return acquisition.Selection{}, s.err
}

func TestCandidateAPIErrorMappingNeverReturnsRawErrors(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{errors.New("RAW_ERROR_SECRET_SENTINEL"), 503},
		{acquisition.ErrSnapshotExpired, 409},
		{acquisition.ErrConflict, 409},
		{acquisition.ErrInvalidID, 400},
		{acquisition.ErrNotFound, 404},
		{acquisition.ErrCancelled, 408},
	} {
		handler := (StatusHandler{AcquisitionCandidates: failingCandidateService{test.err}}).Routes()
		response := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions/job-1/selection", `{"candidate_handle":"`+strings.Repeat("a", 64)+`"}`)
		if response.Code != test.status {
			t.Fatalf("error mapping %v = %d/%s", test.err, response.Code, response.Body.String())
		}
		assertPrivateCandidateData(t, response)
	}
}
