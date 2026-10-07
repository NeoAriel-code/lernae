package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"lernae/internal/acquisition"
	"lernae/internal/database"
	"lernae/internal/jobs"
)

// Diagnostics deliberately classify rather than echo request-controlled data.
// Even an unrelated loopback client cannot make a test print keys, queries,
// authenticated URLs, arbitrary User-Agent text or connection addresses.
func prowlarrRequestClasses(r *http.Request) (method, path, agent, peer string) {
	method = "other"
	if r.Method == http.MethodGet || r.Method == http.MethodPost {
		method = r.Method
	}
	path = "other"
	if r.URL.Path == "/" || r.URL.Path == "/prefix/api/v1/search" {
		path = r.URL.Path
	}
	agent = "other"
	switch ua := r.UserAgent(); {
	case ua == "":
		agent = "absent"
	case ua == "Go-http-client/1.1":
		agent = "go-standard"
	case strings.HasPrefix(ua, "curl/"):
		agent = "curl"
	case strings.HasPrefix(ua, "python-requests/"):
		agent = "python-requests"
	case strings.HasPrefix(ua, "Mozilla/"):
		agent = "browser-like"
	}
	peer = "non-loopback-or-unparsed"
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && net.ParseIP(host).IsLoopback() {
		peer = "loopback-anonymized"
	}
	return
}

// On Linux, attribute an unexpected loopback connection while it is open.
// Read only socket tables and fd symlink metadata: never process arguments,
// environments, credentials or file contents. Output is IDs/allowlisted classes.
func prowlarrLoopbackOwner(r *http.Request) (pid int, class string) {
	class = "unattributed"
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if err != nil || !net.ParseIP(host).Equal(net.IPv4(127, 0, 0, 1)) || !ok {
		return
	}
	localHost, localPort, err := net.SplitHostPort(local.String())
	if err != nil || !net.ParseIP(localHost).Equal(net.IPv4(127, 0, 0, 1)) {
		return
	}
	source, err := strconv.Atoi(port)
	if err != nil {
		return
	}
	destination, err := strconv.Atoi(localPort)
	if err != nil {
		return
	}
	table, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return
	}
	var inode string
	for _, line := range strings.Split(string(table), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		from, to := strings.Split(fields[1], ":"), strings.Split(fields[2], ":")
		if len(from) != 2 || len(to) != 2 || from[0] != "0100007F" || to[0] != "0100007F" {
			continue
		}
		fromPort, e1 := strconv.ParseUint(from[1], 16, 16)
		toPort, e2 := strconv.ParseUint(to[1], 16, 16)
		if e1 == nil && e2 == nil && int(fromPort) == source && int(toPort) == destination {
			inode = fields[9]
			break
		}
	}
	if inode == "" {
		return
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, process := range processes {
		owner, err := strconv.Atoi(process.Name())
		if err != nil || !process.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", process.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", process.Name(), "fd", fd.Name()))
			if err != nil || link != "socket:["+inode+"]" {
				continue
			}
			pid, class = owner, "other-executable"
			exe, err := os.Readlink(filepath.Join("/proc", process.Name(), "exe"))
			if err == nil {
				switch name := filepath.Base(exe); name {
				case "node", "go", "gentle-ai", "gentle-pi", "gentle-shell", "moshi-hook", "pi", "python3", "chrome", "chromium", "firefox", "curl":
					class = name
				default:
					if strings.HasSuffix(name, ".test") {
						class = "go-test-binary"
					}
				}
			}
			return
		}
	}
	return
}

func TestProwlarrExistingAPIExactDurableSelectionPrivateDTOAndUnavailableExecution(t *testing.T) {
	for _, malicious := range []string{"", "https://tracker.invalid/PRIVATE_URL", "PRIVATE_APIKEY_12345", "Title PRIVATE_INSTANCE.invalid", "Title\nSECRET", "magnet:?xt=PRIVATE_MAGNET", "Real.Release.2026.1080p"} {
		t.Run(malicious, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			key := "PRIVATE_APIKEY_12345"
			guid := "https://tracker.invalid/PRIVATE_GUID_PASSKEY"
			var searches atomic.Int32
			var response []byte
			remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				searches.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/prefix/api/v1/search" || r.Header.Get("X-Api-Key") != key || r.URL.Query().Get("query") != "Exact Catalog Title" {
					method, path, agent, peer := prowlarrRequestClasses(r)
					pid, executable := prowlarrLoopbackOwner(r)
					t.Errorf("unexpected search contract: method=%s path=%s key_matches=%t query_matches=%t user_agent_class=%s remote_class=%s source_pid=%d source_is_test=%t source_is_parent=%t executable_class=%s", method, path, r.Header.Get("X-Api-Key") == key, r.URL.Query().Get("query") == "Exact Catalog Title", agent, peer, pid, pid == os.Getpid(), pid == os.Getppid(), executable)
				}
				_, _ = w.Write(response)
			}))
			defer remote.Close()
			// The listener is already bound, but no handler runs until the entire
			// response is immutable. This also covers unexpected early requests.
			remoteURL := "http://" + remote.Listener.Addr().String()
			proxy := remoteURL + "/prefix/2/download?apikey=" + key + "&link=PRIVATE_PROTECTED_LINK&file=Release"
			if strings.Contains(malicious, "PRIVATE_INSTANCE") {
				malicious = "Title " + strings.TrimPrefix(remoteURL, "http://")
			}
			response, _ = json.Marshal([]map[string]any{{"indexerId": 2, "guid": guid, "title": malicious, "protocol": "torrent", "downloadUrl": proxy, "size": 1024}})
			remote.Start()
			config := acquisition.ProwlarrConfig{Enabled: true, BaseURL: remote.URL + "/prefix", APIKey: key, ReferenceDirectory: filepath.Join(base, "config", "lernae", "acquisition", "prowlarr")}
			provider, err := acquisition.NewProwlarr(config)
			if err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(base, "api.db")
			db, err := database.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, query := range []string{
				`INSERT INTO works (id,medium,work_type,title) VALUES ('work-1','video','open-ended','Exact Catalog Title')`,
				`INSERT INTO editions (id,work_id,format) VALUES ('edition-1','work-1','video')`,
			} {
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
			registry, _ := acquisition.NewRegistry(provider)
			selections := acquisition.NewService(jobService, acquisition.NewSQLiteRepository(db), registry)
			core := acquisition.NewExecutionService(ctx, jobService, acquisition.NewSQLiteRepository(db), nil, nil)
			defer core.Close()
			handler := WithAcquisitionExecution(StatusHandler{Acquisitions: jobService, AcquisitionCandidates: selections}.Routes(), core)
			created := acquisitionRequest(handler, "POST", "/api/v1/acquisitions", `{"edition_id":"edition-1"}`)
			var accepted AcquisitionAcceptedResponse
			if json.Unmarshal(created.Body.Bytes(), &accepted) != nil || created.Code != 202 {
				t.Fatal("existing create route failed")
			}
			path := "/api/v1/acquisitions/" + accepted.OperationID
			discovered := acquisitionRequest(handler, "GET", path+"/candidates", "")
			var candidates AcquisitionCandidatesResponse
			wantRealTitle := malicious == "Real.Release.2026.1080p"
			if json.Unmarshal(discovered.Body.Bytes(), &candidates) != nil || discovered.Code != 200 || len(candidates.Candidates) != 1 || candidates.Candidates[0].ProviderID != "prowlarr" || (wantRealTitle && candidates.Candidates[0].Title != malicious) || (!wantRealTitle && !strings.HasPrefix(candidates.Candidates[0].Title, "Prowlarr release ")) {
				t.Fatalf("productive sanitized discovery=%d/%s", discovered.Code, discovered.Body.String())
			}
			selected := acquisitionRequest(handler, "POST", path+"/selection", `{"candidate_handle":"`+candidates.Candidates[0].CandidateHandle+`"}`)
			if selected.Code != 200 {
				t.Fatal("existing select route failed")
			}
			stored, err := selections.GetSelection(ctx, accepted.OperationID)
			if err != nil || stored.Candidate.JobID != accepted.OperationID || stored.Candidate.EditionID != "edition-1" {
				t.Fatal("exact target provenance lost")
			}
			freshProvider, _ := acquisition.NewProwlarr(config)
			private, err := freshProvider.Lookup(ctx, stored.Candidate.Option.ExecutionRef)
			if err != nil || private.GUID != guid || private.Protocol != acquisition.ProwlarrProtocolTorrent || private.CandidateID != stored.Candidate.Option.ID || !private.HasProxyLocator() {
				t.Fatal("restart exact private lookup failed")
			}
			otherDB, err := database.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer otherDB.Close()
			restarted := acquisition.NewService(jobs.NewService(jobs.NewSQLiteRepository(otherDB)), acquisition.NewSQLiteRepository(otherDB), nil)
			read, err := restarted.GetSelection(ctx, accepted.OperationID)
			if err != nil || read != stored {
				t.Fatal("durable selection depended on discovery cache/provider")
			}
			if searches.Load() != 1 {
				t.Fatalf("selection or direct lookup repeated remote search: requests=%d, want=1", searches.Load())
			}
			executed := acquisitionRequest(handler, "POST", path+"/execute", "")
			if executed.Code != 503 {
				t.Fatalf("discovery provider gained execution: %d", executed.Code)
			}
			job, _ := jobService.GetAcquisition(ctx, accepted.OperationID)
			if job.Status != jobs.StatusQueued {
				t.Fatal("unavailable execution advanced lifecycle")
			}
			var reservations int
			if err := db.QueryRow(`SELECT count(*) FROM acquisition_executions`).Scan(&reservations); err != nil || reservations != 0 {
				t.Fatal("discovery-only selection created execution reservation")
			}
			for _, text := range []string{discovered.Body.String(), selected.Body.String(), executed.Body.String(), acquisitionRequest(handler, "GET", path+"/selection", "").Body.String()} {
				for _, secret := range []string{key, remote.URL, proxy, guid, "PRIVATE_GUID_PASSKEY", "PRIVATE_PROTECTED_LINK", stored.Candidate.Option.ExecutionRef, "execution_ref"} {
					if strings.Contains(text, secret) {
						t.Fatal("public acquisition DTO leaked provider-private data")
					}
				}
			}
		})
	}
}
