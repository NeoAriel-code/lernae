package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"lernae/internal/acquisition"
	"lernae/internal/database"
	"lernae/internal/jobs"
)

func TestQBittorrentAPIExactPrivateExecutionAndRemoteOnlySuccess(t *testing.T) {
	for _, mode := range []string{"accepted", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var downloads, adds atomic.Int32
			var tagMu sync.Mutex
			tag := ""
			info := []byte("d6:lengthi4e4:name4:test12:piece lengthi16384e6:pieces20:12345678901234567890e")
			torrent := append(append([]byte("d4:info"), info...), 'e')
			digest := sha1.Sum(info)
			hash := hex.EncodeToString(digest[:])
			qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "PRIVATE_COOKIE", Path: "/"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/app/version":
					_, _ = io.WriteString(w, "v5.0.0")
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.2")
				case "/api/v2/torrents/add":
					adds.Add(1)
					reader, err := r.MultipartReader()
					if err != nil {
						t.Error(err)
						return
					}
					for {
						part, err := reader.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Error(err)
							return
						}
						data, _ := io.ReadAll(part)
						if part.FormName() == "tags" {
							tagMu.Lock()
							tag = string(data)
							tagMu.Unlock()
						}
					}
					if mode == "ambiguous" {
						_, _ = io.WriteString(w, "PRIVATE_RAW_REMOTE_ERROR")
					} else {
						_, _ = io.WriteString(w, "Ok.")
					}
				case "/api/v2/torrents/info":
					tagMu.Lock()
					ownedTag := tag
					tagMu.Unlock()
					_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "tags": ownedTag, "state": "stalledUP", "total_size": 4, "size": 4, "completed": 4, "amount_left": 0, "progress": 1}})
				default:
					t.Error("unexpected qBit command")
				}
			}))
			defer qbit.Close()
			var prowlarrURL string
			prowlarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/search":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"indexerId": 4, "guid": "PRIVATE_GUID", "title": "Exact release", "protocol": "torrent", "downloadUrl": prowlarrURL + "/api/v1/indexer/4/download?link=ProtectedPrivateLink&file=private.torrent&apikey=PRIVATE_API_KEY"}})
				case "/api/v1/indexer/4/download":
					downloads.Add(1)
					_, _ = w.Write(torrent)
				default:
					t.Error("fallback request")
				}
			}))
			defer prowlarr.Close()
			prowlarrURL = prowlarr.URL
			p, err := acquisition.NewProwlarr(acquisition.ProwlarrConfig{Enabled: true, BaseURL: prowlarr.URL, APIKey: "PRIVATE_API_KEY", ReferenceDirectory: filepath.Join(t.TempDir(), "lernae", "acquisition", "prowlarr")})
			if err != nil {
				t.Fatal(err)
			}
			q, err := acquisition.NewQBittorrent(acquisition.QBittorrentConfig{Enabled: true, BaseURL: qbit.URL, Username: "private-operator", Password: "PRIVATE_PASSWORD"}, p)
			if err != nil {
				t.Fatal(err)
			}
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
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
			discoveryRegistry, _ := acquisition.NewRegistry(p)
			selections := acquisition.NewService(jobService, acquisition.NewSQLiteRepository(db), discoveryRegistry)
			discovered, err := selections.Discover(ctx, job.ID)
			if err != nil || len(discovered.Candidates) != 1 {
				t.Fatal("controlled discovery failed")
			}
			selected, err := selections.Select(ctx, job.ID, discovered.Candidates[0].Handle)
			if err != nil || downloads.Load() != 0 || adds.Load() != 0 {
				t.Fatal("selection executed")
			}
			registry, _ := acquisition.NewExecutionRegistry(acquisition.NewProwlarrExecutionResolver(p))
			core := acquisition.NewExecutionService(ctx, jobService, acquisition.NewSQLiteRepository(db), registry, q)
			defer core.Close()
			handler := WithAcquisitionExecution(StatusHandler{Acquisitions: jobService, AcquisitionCandidates: selections}.Routes(), core)
			path := "/api/v1/acquisitions/" + job.ID
			response := acquisitionRequest(handler, http.MethodPost, path+"/execute", "")
			want := http.StatusAccepted
			if mode == "ambiguous" {
				want = http.StatusConflict
			}
			if response.Code != want {
				t.Fatalf("execute HTTP = %d", response.Code)
			}
			core.Wait()
			private := []string{"PRIVATE_GUID", "PRIVATE_API_KEY", "PRIVATE_PASSWORD", "PRIVATE_COOKIE", "PRIVATE_RAW_REMOTE_ERROR", "private-operator", "ProtectedPrivateLink", prowlarr.URL, qbit.URL, hash, selected.Candidate.Option.ExecutionRef}
			for _, suffix := range []string{"", "/selection"} {
				projected := acquisitionRequest(handler, http.MethodGet, path+suffix, "")
				for _, value := range private {
					if strings.Contains(response.Body.String()+projected.Body.String(), value) {
						t.Fatal("public response leaked private executor data")
					}
				}
			}
			loaded, err := jobService.GetAcquisition(ctx, job.ID)
			wantStatus := jobs.StatusSucceeded
			if mode == "ambiguous" {
				wantStatus = jobs.StatusFailed
			}
			if err != nil || loaded.Status != wantStatus {
				t.Fatal("incorrect public Job lifecycle")
			}
			for _, table := range []string{"assets", "asset_locations"} {
				var count int
				if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil || count != 0 {
					t.Fatal("remote completion materialized inventory")
				}
			}
			retry := acquisitionRequest(handler, http.MethodPost, path+"/execute", "")
			if retry.Code != want || downloads.Load() != 1 || adds.Load() != 1 {
				t.Fatal("HTTP retry redispatched or refetched")
			}
		})
	}
}
