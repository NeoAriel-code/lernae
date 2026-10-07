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

	"lernae/internal/acquisition"
	"lernae/internal/database"
	"lernae/internal/jobs"
)

type executionAPIFunc func(context.Context, string) (acquisition.Reservation, error)

func (f executionAPIFunc) Execute(ctx context.Context, id string) (acquisition.Reservation, error) {
	return f(ctx, id)
}

func TestExecutionHTTPValidationAndSafeErrors(t *testing.T) {
	calls := 0
	service := executionAPIFunc(func(context.Context, string) (acquisition.Reservation, error) {
		calls++
		return acquisition.Reservation{}, errors.New("RAW_ERROR_SECRET_SENTINEL PRIVATE_EXECUTION_SENTINEL")
	})
	handler := WithAcquisitionExecution(StatusHandler{}.Routes(), service)
	for _, test := range []struct {
		method string
		suffix string
		body   string
		status int
	}{
		{http.MethodGet, "", "", 405},
		{http.MethodPut, "", "", 405},
		{http.MethodPost, "?candidate=other", "", 400},
		{http.MethodPost, "", `{}`, 400},
		{http.MethodPost, "", `{"execution_ref":"PRIVATE_EXECUTION_SENTINEL"}`, 400},
		{http.MethodPost, "", `{"candidate_handle":"other"}`, 400},
		{http.MethodPost, "", " ", 400},
		{http.MethodPost, "", strings.Repeat("x", 5000), 400},
	} {
		response := acquisitionRequest(handler, test.method, "/api/v1/acquisitions/job-1/execute"+test.suffix, test.body)
		if response.Code != test.status {
			t.Fatalf("validation response = %d/%s, want %d", response.Code, response.Body.String(), test.status)
		}
		if test.status == 405 && response.Header().Get("Allow") != http.MethodPost {
			t.Fatal("missing POST Allow")
		}
		assertPrivateCandidateData(t, response)
	}
	if calls != 0 {
		t.Fatal("invalid input reached execution service")
	}
	response := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions/job-1/execute", "")
	if response.Code != 503 || calls != 1 {
		t.Fatalf("unsafe infrastructure response = %d/%s", response.Code, response.Body.String())
	}
	assertPrivateCandidateData(t, response)
	// Wrapping must preserve the complete existing route tree.
	if response := acquisitionRequest(handler, http.MethodGet, "/api/v1/health", ""); response.Code != 200 {
		t.Fatal("execution wrapper shadowed existing routes")
	}
}

type executionAPIResolver struct {
	fail bool
}

func (executionAPIResolver) ID() string { return "source" }
func (r executionAPIResolver) Resolve(_ context.Context, selection acquisition.Selection) (acquisition.ExecutionPlan, error) {
	if r.fail {
		return acquisition.ExecutionPlan{}, errors.New("RAW_ERROR_SECRET_SENTINEL PRIVATE_EXECUTION_SENTINEL")
	}
	return acquisition.PlanForSelection(selection), nil
}

type executionAPIExecutor struct {
	calls int
	fail  bool
}

func (e *executionAPIExecutor) Dispatch(_ context.Context, plan acquisition.ExecutionPlan) (acquisition.DispatchResult, error) {
	e.calls++
	if plan.ExecutionRef != "PRIVATE_EXECUTION_SENTINEL" || plan.ProviderID != "source" || plan.CandidateID != "first" {
		return acquisition.DispatchResult{}, errors.New("wrong provenance")
	}
	if e.fail {
		return acquisition.DispatchResult{}, errors.New("RAW_ERROR_SECRET_SENTINEL PRIVATE_EXECUTION_SENTINEL")
	}
	return acquisition.DispatchResult{Decision: acquisition.DispatchAccepted, Execution: executionAPIWork{}}, nil
}

type executionAPIWork struct{}

func (executionAPIWork) Wait(context.Context, func(acquisition.Progress) error) (acquisition.Completion, error) {
	return acquisition.CompletionSucceeded, nil
}

func TestExecutionAPIRealSQLiteExactSelectionPrivacyAndRetries(t *testing.T) {
	for _, mode := range []string{"missing selection", "provider absent", "executor absent", "unresolved", "dispatch error", "accepted"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "execution-api.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, query := range []string{
				`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
				`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
			} {
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
			job, err := jobService.CreateAcquisition(ctx, "edition-1")
			if err != nil {
				t.Fatal(err)
			}
			discoveryRegistry, _ := acquisition.NewRegistry(privateOptionsProvider())
			selections := acquisition.NewService(jobService, acquisition.NewSQLiteRepository(db), discoveryRegistry)
			if mode != "missing selection" {
				discovery, err := selections.Discover(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := selections.Select(ctx, job.ID, discovery.Candidates[0].Handle); err != nil {
					t.Fatal(err)
				}
			}
			registry, _ := acquisition.NewExecutionRegistry(executionAPIResolver{fail: mode == "unresolved"})
			executor := &executionAPIExecutor{fail: mode == "dispatch error"}
			var adapter acquisition.Executor = executor
			if mode == "provider absent" {
				registry = nil
			}
			if mode == "executor absent" {
				adapter = nil
			}
			core := acquisition.NewExecutionService(ctx, jobService, acquisition.NewSQLiteRepository(db), registry, adapter)
			defer core.Close()
			handler := WithAcquisitionExecution(StatusHandler{Acquisitions: jobService, AcquisitionCandidates: selections}.Routes(), core)
			path := "/api/v1/acquisitions/" + job.ID
			response := acquisitionRequest(handler, http.MethodPost, path+"/execute", "")
			want := 409
			if mode == "provider absent" || mode == "executor absent" {
				want = 503
			}
			if mode == "accepted" {
				want = 202
			}
			if response.Code != want {
				t.Fatalf("execution response = %d/%s, want %d", response.Code, response.Body.String(), want)
			}
			assertPrivateCandidateData(t, response)
			core.Wait()
			if mode == "accepted" || mode == "dispatch error" {
				var receipt AcquisitionExecutionResponse
				if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.ExecutionID == "" || receipt.OperationID != job.ID {
					t.Fatalf("durable receipt = %#v, %v", receipt, err)
				}
				retry := acquisitionRequest(handler, http.MethodPost, path+"/execute", "")
				if retry.Code != response.Code || retry.Body.String() != response.Body.String() || executor.calls != 1 {
					t.Fatalf("retry = %d/%s; calls %d", retry.Code, retry.Body.String(), executor.calls)
				}
				assertPrivateCandidateData(t, retry)
			} else if executor.calls != 0 {
				t.Fatal("preaccept error dispatched")
			}
			for _, suffix := range []string{"", "/selection", "/candidates"} {
				assertPrivateCandidateData(t, acquisitionRequest(handler, http.MethodGet, path+suffix, ""))
			}
			for _, id := range []string{"missing", "bad.id"} {
				response := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions/"+id+"/execute", "")
				want := 404
				if id == "bad.id" {
					want = 400
				}
				if response.Code != want {
					t.Fatalf("identifier error = %d/%s", response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestExecutionRouteMissingServiceIsSafe(t *testing.T) {
	// Server composition will wrap Routes without changing the existing handler.
	handler := WithAcquisitionExecution(StatusHandler{}.Routes(), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions/job-1/execute", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("execute returned %d, want safe 503", response.Code)
	}
}
