package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lernae/internal/acquisition"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/jobs"
)

func TestLocalAcquisitionExistingAPIStagesExactBytesAndKeepsPathsPrivate(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	source, staging := filepath.Join(base, "PRIVATE_SOURCE"), filepath.Join(base, "PRIVATE_STAGING")
	for _, path := range []string{source, staging, filepath.Join(source, "edition-1")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := bytes.Repeat([]byte("real API file bytes"), 5000)
	if err := os.WriteFile(filepath.Join(source, "edition-1", "PRIVATE_FILENAME"), data, 0600); err != nil {
		t.Fatal(err)
	}
	local, err := acquisition.OpenLocal(source, staging)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	db, err := database.Open(ctx, filepath.Join(base, "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1','literature','novel','Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1','work-1','epub')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	registry, _ := acquisition.NewRegistry(local)
	selections := acquisition.NewService(jobService, acquisition.NewSQLiteRepository(db), registry)
	executionRegistry, _ := acquisition.NewExecutionRegistry(local)
	core := acquisition.NewExecutionService(ctx, jobService, acquisition.NewSQLiteRepository(db), executionRegistry, local)
	defer core.Close()
	handler := WithAcquisitionExecution(StatusHandler{Acquisitions: jobService, AcquisitionCandidates: selections}.Routes(), core)
	create := acquisitionRequest(handler, http.MethodPost, "/api/v1/acquisitions", `{"edition_id":"edition-1"}`)
	var accepted AcquisitionAcceptedResponse
	if err := json.Unmarshal(create.Body.Bytes(), &accepted); err != nil || create.Code != 202 {
		t.Fatalf("create=%d/%s", create.Code, create.Body.String())
	}
	path := "/api/v1/acquisitions/" + accepted.OperationID
	response := acquisitionRequest(handler, http.MethodGet, path+"/candidates", "")
	var discovered AcquisitionCandidatesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &discovered); err != nil || response.Code != 200 || len(discovered.Candidates) != 1 {
		t.Fatalf("discover=%d/%s", response.Code, response.Body.String())
	}
	private := func(text string) {
		t.Helper()
		for _, marker := range []string{source, staging, "PRIVATE_FILENAME", "execution_ref"} {
			if strings.Contains(text, marker) {
				t.Fatalf("public data leaked %q", marker)
			}
		}
	}
	private(response.Body.String())
	storedOptions, err := local.Discover(ctx, acquisition.Target{Edition: domain.Edition{ID: "edition-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range storedOptions {
		if strings.Contains(response.Body.String(), `"`+option.ExecutionRef+`"`) {
			t.Fatal("reference exposed as public value")
		}
	}
	response = acquisitionRequest(handler, http.MethodPost, path+"/selection", `{"candidate_handle":"`+discovered.Candidates[0].CandidateHandle+`"}`)
	if response.Code != 200 {
		t.Fatalf("select=%d/%s", response.Code, response.Body.String())
	}
	private(response.Body.String())
	for _, body := range []string{`{"source_path":"` + source + `"}`, `{"execution_ref":"private"}`} {
		response = acquisitionRequest(handler, http.MethodPost, path+"/execute", body)
		if response.Code != 400 {
			t.Fatal("HTTP accepted filesystem authority")
		}
		private(response.Body.String())
	}
	response = acquisitionRequest(handler, http.MethodPost, path+"/execute", "")
	var receipt AcquisitionExecutionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || response.Code != 202 || receipt.Outcome != "accepted" {
		t.Fatalf("execute=%d/%s", response.Code, response.Body.String())
	}
	private(response.Body.String())
	core.Wait()
	loaded, err := jobService.GetAcquisition(ctx, accepted.OperationID)
	if err != nil || loaded.Status != jobs.StatusSucceeded || loaded.ProgressCurrent != int64(len(data)) || loaded.ProgressTotal != int64(len(data)) {
		t.Fatalf("job=%v err=%v", loaded, err)
	}
	copied, err := os.ReadFile(filepath.Join(staging, receipt.ExecutionID, "content"))
	if err != nil || !bytes.Equal(copied, data) {
		t.Fatal("real staging mismatch")
	}
	for _, suffix := range []string{"", "/selection", "/candidates"} {
		private(acquisitionRequest(handler, http.MethodGet, path+suffix, "").Body.String())
	}
	retry := acquisitionRequest(handler, http.MethodPost, path+"/execute", "")
	if retry.Code != response.Code || retry.Body.String() != response.Body.String() {
		t.Fatal("retry changed stable receipt")
	}
}
