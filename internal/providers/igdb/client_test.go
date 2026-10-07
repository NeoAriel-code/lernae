package igdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lernae/internal/config"
	"lernae/internal/domain"
)

const (
	testClientID     = "synthetic-client-id"
	testClientSecret = "synthetic-client-secret"
	testAccessToken  = "synthetic-access-token"
)

func TestSearchRequiresCompleteCredentialsWithoutMakingRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name        string
		credentials config.IGDBCredentials
	}{
		{name: "missing"},
		{name: "partial", credentials: config.IGDBCredentials{ClientID: testClientID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := newSource(test.credentials, server.URL, server.URL, server.Client())
			_, err := source.SearchWorks(context.Background(), "Soul Calibur")
			if !errors.Is(err, ErrConfiguration) {
				t.Fatalf("SearchWorks error = %v, want safe configuration error", err)
			}
			if strings.Contains(err.Error(), testClientSecret) {
				t.Fatal("configuration error exposed credential content")
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests without credentials = %d, want 0", got)
	}
}

func TestProviderFormattingNeverRevealsCredentialsOrCachedToken(t *testing.T) {
	source := newSource(testCredentials(), "http://127.0.0.1", "http://127.0.0.1/v4", &http.Client{})
	source.token = testAccessToken
	for _, formatted := range []string{fmt.Sprintf("%v", source), fmt.Sprintf("%+v", source), fmt.Sprintf("%#v", source)} {
		for _, privateValue := range []string{testClientID, testClientSecret, testAccessToken} {
			if strings.Contains(formatted, privateValue) {
				t.Fatal("provider formatting exposed private data")
			}
		}
	}
}

func TestSearchAuthenticatesWithClientCredentialsAndMapsSoulcalibur(t *testing.T) {
	var authCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			authCalls.Add(1)
			if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("OAuth request method/content-type = %q/%q", request.Method, request.Header.Get("Content-Type"))
			}
			if err := request.ParseForm(); err != nil {
				t.Errorf("parse OAuth form: %v", err)
			}
			if request.Form.Get("client_id") != testClientID || request.Form.Get("client_secret") != testClientSecret || request.Form.Get("grant_type") != "client_credentials" {
				t.Errorf("OAuth form did not contain the synthetic client-credentials grant")
			}
			writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600, "token_type": "bearer"})
		case "/v4/games":
			if request.Method != http.MethodPost {
				t.Errorf("IGDB API method = %q, want POST", request.Method)
			}
			if request.Header.Get("Content-Type") != "text/plain" {
				t.Errorf("IGDB API content-type = %q, want text/plain", request.Header.Get("Content-Type"))
			}
			if request.Header.Get("Client-ID") != testClientID || request.Header.Get("Authorization") != "Bearer "+testAccessToken {
				t.Errorf("IGDB API authentication headers were not applied")
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read APICalypse request: %v", err)
			}
			statement := string(body)
			for _, required := range []string{
				"fields id,name,first_release_date,summary,cover.image_id,platforms.name,platforms.slug;",
				"where version_parent = null & name != null;",
				"search \"Soul Calibur\";",
				"limit 10;",
			} {
				if !strings.Contains(statement, required) {
					t.Errorf("APICalypse statement %q does not include fixed clause %q", statement, required)
				}
			}
			writeJSON(t, w, []map[string]any{{
				"id": 1001, "name": "Soulcalibur II", "first_release_date": time.Date(2002, time.August, 26, 0, 0, 0, 0, time.UTC).Unix(),
				"summary": "A synthetic test fixture.", "cover": map[string]any{"image_id": "co1abc"},
				"platforms": []map[string]any{{"name": "Nintendo GameCube", "slug": "gamecube"}, {"name": "Arcade", "slug": "arcade"}},
			}})
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	results, err := source.SearchWorks(context.Background(), "Soul Calibur")
	if err != nil {
		t.Fatalf("SearchWorks: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	result := results[0]
	if result.Provider != "igdb" || result.ExternalID != "1001" || result.Title != "Soulcalibur II" {
		t.Fatalf("normalized identity/title = %#v", result)
	}
	if result.Medium != domain.MediumGame || result.WorkType != "game" {
		t.Fatalf("normalized medium/work type = %q/%q", result.Medium, result.WorkType)
	}
	wantDate := time.Date(2002, time.August, 26, 0, 0, 0, 0, time.UTC)
	if result.ReleaseDate == nil || !result.ReleaseDate.Equal(wantDate) || result.ReleaseYear != 2002 {
		t.Fatalf("normalized release date/year = %v/%d", result.ReleaseDate, result.ReleaseYear)
	}
	if result.Summary != "A synthetic test fixture." || result.CoverReference != "https://images.igdb.com/igdb/image/upload/t_cover_big/co1abc.jpg" {
		t.Fatalf("normalized summary/cover reference = %q/%q", result.Summary, result.CoverReference)
	}
	if len(result.Platforms) != 2 || result.Platforms[0] != (domain.Platform{Name: "Nintendo GameCube", Slug: "gamecube"}) {
		t.Fatalf("normalized platforms = %#v", result.Platforms)
	}
	if authCalls.Load() != 1 {
		t.Fatalf("OAuth calls = %d, want 1", authCalls.Load())
	}
}

func TestGetWorkDetailsCanonicalizesChildAndAggregatesDirectPlatforms(t *testing.T) {
	var authCalls atomic.Int32
	var apiStatements []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			authCalls.Add(1)
			writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
		case "/v4/games":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read APICalypse request: %v", err)
			}
			statement := string(body)
			apiStatements = append(apiStatements, statement)
			switch {
			case strings.Contains(statement, "where id = 227987;"):
				writeJSON(t, w, []map[string]any{{"id": 227987, "name": "Soulcalibur II (GameCube)", "parent_game": map[string]any{"id": 1565}}})
			case strings.Contains(statement, "where id = 1565;"):
				writeJSON(t, w, []map[string]any{{
					"id": 1565, "name": "Soulcalibur II", "first_release_date": time.Date(2002, time.August, 26, 0, 0, 0, 0, time.UTC).Unix(),
					"summary": "Canonical Work summary.", "cover": map[string]any{"image_id": "coSoulcalibur"},
					"platforms": []map[string]any{{"name": "Arcade", "slug": "arcade"}, {"name": "PlayStation 2", "slug": "ps2"}},
				}})
			case strings.Contains(statement, "where parent_game = 1565;"):
				writeJSON(t, w, []map[string]any{
					{"id": 227989, "parent_game": map[string]any{"id": 1565}, "platforms": []map[string]any{{"name": "Xbox", "slug": "xbox"}}},
					{"id": 227987, "parent_game": map[string]any{"id": 1565}, "platforms": []map[string]any{{"name": "Nintendo GameCube", "slug": "nintendo-gamecube"}}},
					{"id": 227986, "parent_game": map[string]any{"id": 1565}, "platforms": []map[string]any{{"name": "PlayStation 2", "slug": "ps2"}}},
					{"id": 991, "parent_game": map[string]any{"id": 999}, "platforms": []map[string]any{{"name": "Dreamcast", "slug": "dreamcast"}}},
				})
			default:
				t.Errorf("unexpected APICalypse statement %q", statement)
				http.Error(w, "unexpected query", http.StatusBadRequest)
			}
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	details, err := source.GetWorkDetails(context.Background(), "igdb", "227987")
	if err != nil {
		t.Fatalf("GetWorkDetails: %v", err)
	}
	if details.Provider != "igdb" || details.ExternalID != "1565" || details.Title != "Soulcalibur II" {
		t.Fatalf("canonical identity/title = %#v, want igdb:1565 Soulcalibur II", details)
	}
	if details.Medium != domain.MediumGame || details.WorkType != "game" || details.Summary != "Canonical Work summary." {
		t.Fatalf("normalized Work fields = %#v", details)
	}
	if details.ReleaseDate == nil || details.ReleaseDate.Year() != 2002 || details.ReleaseYear != 2002 || details.CoverReference != "https://images.igdb.com/igdb/image/upload/t_cover_big/coSoulcalibur.jpg" {
		t.Fatalf("release/cover fields = %#v", details)
	}
	platforms := make(map[string]domain.PlatformCandidate, len(details.PlatformCandidates))
	for _, candidate := range details.PlatformCandidates {
		platforms[candidate.Platform.Slug] = candidate
	}
	for _, slug := range []string{"arcade", "nintendo-gamecube", "ps2", "xbox"} {
		if _, ok := platforms[slug]; !ok {
			t.Errorf("platform candidates %#v omit %q", details.PlatformCandidates, slug)
		}
	}
	if _, ok := platforms["dreamcast"]; ok {
		t.Fatalf("unrelated parent platform was included: %#v", details.PlatformCandidates)
	}
	if got := len(platforms["ps2"].Provenance); got != 2 {
		t.Errorf("deduplicated PS2 provenance count = %d, want canonical Work and child evidence", got)
	}
	if len(apiStatements) != 3 {
		t.Fatalf("IGDB game request count = %d, want fixed child lookup + canonical Work + grouped children", len(apiStatements))
	}
	for _, statement := range apiStatements {
		for _, forbidden := range []string{"ports", "version_parent", "game_versions"} {
			if strings.Contains(statement, forbidden) {
				t.Errorf("Work Details query depends on forbidden relation %q: %s", forbidden, statement)
			}
		}
	}
	if authCalls.Load() != 1 {
		t.Errorf("OAuth calls = %d, want the shared source token reused across details requests", authCalls.Load())
	}
}

func TestGetWorkDetailsForCanonicalWorkKeepsDirectPlatformsWithoutChildrenAndUsesTwoRequests(t *testing.T) {
	var apiCalls atomic.Int32
	server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
		case "/v4/games":
			body, _ := io.ReadAll(request.Body)
			apiCalls.Add(1)
			switch {
			case strings.Contains(string(body), "where id = 1565;"):
				writeJSON(t, w, []map[string]any{{"id": 1565, "name": "Soulcalibur II", "platforms": []map[string]any{{"name": "Arcade", "slug": "arcade"}}}})
			case strings.Contains(string(body), "where parent_game = 1565;"):
				writeJSON(t, w, []any{})
			default:
				t.Errorf("unexpected APICalypse statement %q", string(body))
				http.Error(w, "unexpected query", http.StatusBadRequest)
			}
		default:
			http.NotFound(w, request)
		}
	})

	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	details, err := source.GetWorkDetails(context.Background(), "igdb", "1565")
	if err != nil {
		t.Fatalf("GetWorkDetails: %v", err)
	}
	if details.ExternalID != "1565" || len(details.PlatformCandidates) != 1 || details.PlatformCandidates[0].Platform.Slug != "arcade" {
		t.Fatalf("Work without children details = %#v, want canonical Arcade candidate", details)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Fatalf("IGDB game request count without children = %d, want 2 independent of child count", got)
	}
}

func TestGetWorkDetailsRejectsUnsupportedProviderAndInvalidExternalIDWithoutRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())

	for _, test := range []struct {
		provider   string
		externalID string
		want       error
	}{
		{provider: "romm", externalID: "1565", want: ErrUnsupportedProvider},
		{provider: "igdb", externalID: "1565; delete", want: ErrInvalidExternalID},
		{provider: "igdb", externalID: "0", want: ErrInvalidExternalID},
	} {
		if _, err := source.GetWorkDetails(context.Background(), test.provider, test.externalID); !errors.Is(err, test.want) {
			t.Errorf("GetWorkDetails(%q, %q) error = %v, want %v", test.provider, test.externalID, err, test.want)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests for invalid identities = %d, want 0", got)
	}
}

func TestTokenIsReusedUntilProactiveRefreshBoundary(t *testing.T) {
	var authCalls atomic.Int32
	server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			authCalls.Add(1)
			writeJSON(t, w, map[string]any{"access_token": fmt.Sprintf("token-%d", authCalls.Load()), "expires_in": 100})
		case "/v4/games":
			writeJSON(t, w, []any{})
		default:
			http.NotFound(w, request)
		}
	})
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	source := IGDBMetadataSource{
		credentials: testCredentials(), authURL: server.URL + "/oauth2/token", apiBaseURL: server.URL + "/v4",
		httpClient: server.Client(), requestTimeout: time.Second, now: func() time.Time { return now },
	}
	for range 2 {
		if _, err := source.SearchWorks(context.Background(), "Soulcalibur"); err != nil {
			t.Fatalf("SearchWorks before expiry: %v", err)
		}
	}
	if got := authCalls.Load(); got != 1 {
		t.Fatalf("OAuth calls before refresh boundary = %d, want 1", got)
	}
	now = now.Add(91 * time.Second)
	if _, err := source.SearchWorks(context.Background(), "Soulcalibur"); err != nil {
		t.Fatalf("SearchWorks after proactive refresh boundary: %v", err)
	}
	if got := authCalls.Load(); got != 2 {
		t.Fatalf("OAuth calls after proactive refresh boundary = %d, want 2", got)
	}
}

func TestUnauthorizedGameRequestRefreshesAndRetriesOnce(t *testing.T) {
	var authCalls, apiCalls atomic.Int32
	server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			call := authCalls.Add(1)
			writeJSON(t, w, map[string]any{"access_token": fmt.Sprintf("token-%d", call), "expires_in": 3600})
		case "/v4/games":
			apiCalls.Add(1)
			if request.Header.Get("Authorization") == "Bearer token-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBodyBytes+1))
				return
			}
			writeJSON(t, w, []any{})
		default:
			http.NotFound(w, request)
		}
	})
	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	if _, err := source.SearchWorks(context.Background(), "Soulcalibur"); err != nil {
		t.Fatalf("SearchWorks after one 401: %v", err)
	}
	if authCalls.Load() != 2 || apiCalls.Load() != 2 {
		t.Fatalf("auth/API calls = %d/%d, want 2/2", authCalls.Load(), apiCalls.Load())
	}
}

func TestRepeatedUnauthorizedGameRequestDoesNotLoop(t *testing.T) {
	var authCalls, apiCalls atomic.Int32
	server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			call := authCalls.Add(1)
			writeJSON(t, w, map[string]any{"access_token": fmt.Sprintf("token-%d", call), "expires_in": 3600})
		case "/v4/games":
			apiCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, request)
		}
	})
	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	_, err := source.SearchWorks(context.Background(), "Soulcalibur")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("repeated 401 error = %v, want safe unauthorized error", err)
	}
	if authCalls.Load() != 2 || apiCalls.Load() != 2 {
		t.Fatalf("auth/API calls = %d/%d, want exactly 2/2", authCalls.Load(), apiCalls.Load())
	}
}

func TestConcurrentTokenRequestsUseSingleFlight(t *testing.T) {
	var authCalls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth2/token" {
			http.NotFound(w, request)
			return
		}
		authCalls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
	})
	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	const callers = 16
	start := make(chan struct{})
	var ready, wait sync.WaitGroup
	ready.Add(callers)
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			ready.Done()
			<-start
			if _, err := source.getToken(context.Background()); err != nil {
				t.Errorf("getToken: %v", err)
			}
		}()
	}
	ready.Wait()
	close(start)
	<-entered
	time.Sleep(20 * time.Millisecond)
	close(release)
	wait.Wait()
	if got := authCalls.Load(); got != 1 {
		t.Fatalf("OAuth calls for %d concurrent callers = %d, want 1", callers, got)
	}
}

func TestQueryIsDataAndAPICalypseSyntaxIsRejected(t *testing.T) {
	var statement string
	var statementMu sync.Mutex
	server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/oauth2/token" {
			writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
			return
		}
		body, _ := io.ReadAll(request.Body)
		statementMu.Lock()
		statement = string(body)
		statementMu.Unlock()
		writeJSON(t, w, []any{})
	})
	source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
	if _, err := source.SearchWorks(context.Background(), `Soul Calibur "; limit 500;`); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("unsafe query error = %v, want invalid query", err)
	}
	for _, query := range []string{"", "  \n", "line\nbreak", "line\u2028break", strings.Repeat("x", maxQueryBytes+1)} {
		if _, err := source.SearchWorks(context.Background(), query); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("query %q error = %v, want invalid query", query, err)
		}
	}
	statementMu.Lock()
	gotStatement := statement
	statementMu.Unlock()
	if gotStatement != "" {
		t.Fatal("rejected query reached the API")
	}
	if _, err := source.SearchWorks(context.Background(), "Soul Calibur"); err != nil {
		t.Fatalf("safe query was rejected: %v", err)
	}
	statementMu.Lock()
	gotStatement = statement
	statementMu.Unlock()
	if !strings.Contains(gotStatement, `search "Soul Calibur";`) || strings.Contains(gotStatement, "limit 500") {
		t.Fatal("query did not remain data in the fixed APICalypse statement")
	}
}

func TestProviderMapsStatusAndMalformedResponseErrorsSafely(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"message":"private provider detail"}`, want: ErrRateLimited},
		{name: "server error", status: http.StatusBadGateway, body: `{"message":"private provider detail"}`, want: ErrProviderUnavailable},
		{name: "malformed json", status: http.StatusOK, body: `{not-json`, want: ErrMalformedResponse},
		{name: "oversized body", status: http.StatusOK, body: strings.Repeat("x", maxResponseBodyBytes+1), want: ErrResponseTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/oauth2/token" {
					writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
					return
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
			_, err := source.SearchWorks(context.Background(), "Soulcalibur")
			if !errors.Is(err, test.want) {
				t.Fatalf("SearchWorks error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "private provider detail") || strings.Contains(err.Error(), testAccessToken) {
				t.Fatalf("provider error exposed response or token content: %q", err.Error())
			}
		})
	}
}

func TestAuthenticationErrorsAreSafe(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"private-auth-detail"}`, want: ErrAuthentication},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"private-auth-detail"}`, want: ErrRateLimited},
		{name: "server error", status: http.StatusServiceUnavailable, body: `{"error":"private-auth-detail"}`, want: ErrProviderUnavailable},
		{name: "malformed json", status: http.StatusOK, body: `{not-json`, want: ErrMalformedResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
			_, err := source.SearchWorks(context.Background(), "Soulcalibur")
			if !errors.Is(err, test.want) {
				t.Fatalf("SearchWorks error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "private-auth-detail") || strings.Contains(err.Error(), testClientSecret) {
				t.Fatalf("authentication error exposed private data: %q", err.Error())
			}
		})
	}
}

func TestOAuthDoesNotForwardCredentialsAcrossRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	destination := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		redirectedRequests.Add(1)
		http.NotFound(w, request)
	})
	auth := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, destination.URL+"/capture", http.StatusTemporaryRedirect)
	})
	source := newSource(testCredentials(), auth.URL, destination.URL+"/v4", auth.Client())
	_, err := source.SearchWorks(context.Background(), "Soulcalibur")
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("redirected OAuth error = %v, want safe authentication error", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("OAuth credentials were forwarded to a redirected endpoint")
	}
}

func TestProviderMapsNetworkTimeoutAndCancellationErrors(t *testing.T) {
	t.Run("network", func(t *testing.T) {
		authServer := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
			writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
		})
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		apiURL := "http://" + listener.Addr().String() + "/v4"
		_ = listener.Close()
		source := newSource(testCredentials(), authServer.URL, apiURL, &http.Client{})
		if _, err := source.SearchWorks(context.Background(), "Soulcalibur"); !errors.Is(err, ErrNetwork) {
			t.Fatalf("network error = %v, want safe network error", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		started := make(chan struct{})
		server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/oauth2/token" {
				writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
				return
			}
			close(started)
			time.Sleep(100 * time.Millisecond)
			writeJSON(t, w, []any{})
		})
		source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
		if _, err := source.getToken(context.Background()); err != nil {
			t.Fatalf("prepare synthetic token: %v", err)
		}
		source.requestTimeout = 20 * time.Millisecond
		if _, err := source.SearchWorks(context.Background(), "Soulcalibur"); !errors.Is(err, ErrTimeout) {
			t.Fatalf("timeout error = %v, want safe timeout error", err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed-out API request did not reach the test server")
		}
	})

	t.Run("canceled", func(t *testing.T) {
		started := make(chan struct{})
		server := newJSONServer(t, func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/oauth2/token" {
				writeJSON(t, w, map[string]any{"access_token": testAccessToken, "expires_in": 3600})
				return
			}
			close(started)
			time.Sleep(100 * time.Millisecond)
			writeJSON(t, w, []any{})
		})
		source := newSource(testCredentials(), server.URL, server.URL+"/v4", server.Client())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := source.SearchWorks(ctx, "Soulcalibur")
			done <- err
		}()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, ErrCanceled) {
			t.Fatalf("cancellation error = %v, want safe canceled error", err)
		}
	})
}

func TestProcessLocalAPILimitsBoundRateAndOpenCalls(t *testing.T) {
	source := newSource(testCredentials(), "http://127.0.0.1", "http://127.0.0.1/v4", &http.Client{})
	otherSource := newSource(testCredentials(), "http://127.0.0.1", "http://127.0.0.1/v4", &http.Client{})
	thirdSource := newSource(testCredentials(), "http://127.0.0.1", "http://127.0.0.1/v4", &http.Client{})
	var starts []time.Time
	for index := range 5 {
		current := source
		if index%2 == 1 {
			current = otherSource
		}
		if err := current.enterAPILimit(context.Background()); err != nil {
			t.Fatal(err)
		}
		starts = append(starts, time.Now())
		current.leaveAPILimit()
	}
	if elapsed := starts[4].Sub(starts[0]); elapsed < 900*time.Millisecond {
		t.Fatalf("five request start window = %s, want at least 0.9 seconds for four/sec", elapsed)
	}

	for index := range maxOpenAPIRequests {
		current := source
		if index%2 == 1 {
			current = otherSource
		}
		if err := current.enterAPILimit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	blocked := make(chan error, 1)
	go func() { blocked <- thirdSource.enterAPILimit(context.Background()) }()
	select {
	case err := <-blocked:
		source.leaveAPILimit()
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("API slot beyond the configured open-call limit was admitted")
	case <-time.After(20 * time.Millisecond):
	}
	source.leaveAPILimit()
	if err := <-blocked; err != nil {
		t.Fatalf("request did not enter after one slot was released: %v", err)
	}
	for range maxOpenAPIRequests {
		source.leaveAPILimit()
	}
}

func newSource(credentials config.IGDBCredentials, authURL, apiBaseURL string, client *http.Client) *IGDBMetadataSource {
	parsed, err := url.Parse(authURL)
	if err == nil && (parsed.Path == "" || parsed.Path == "/") {
		authURL = strings.TrimRight(authURL, "/") + "/oauth2/token"
	}
	return New(ClientConfig{
		Credentials: credentials,
		AuthURL:     authURL, APIBaseURL: apiBaseURL,
		HTTPClient: client,
	})
}

func newJSONServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode synthetic response: %v", err)
	}
}

func testCredentials() config.IGDBCredentials {
	return config.IGDBCredentials{ClientID: testClientID, ClientSecret: testClientSecret}
}
