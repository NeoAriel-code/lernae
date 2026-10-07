package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/catalog"
	"lernae/internal/config"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/inventory"
	"lernae/internal/providers"
	"lernae/internal/providers/igdb"
	"lernae/internal/providers/wikidata"
	"lernae/internal/searchcache"
)

type statusClient struct {
	status agent.AgentStatus
	err    error
}

func (client statusClient) GetStatus(context.Context) (agent.AgentStatus, error) {
	return client.status, client.err
}

func TestHealthEndpoint(t *testing.T) {
	server := httptest.NewServer((StatusHandler{}).Routes())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", response.StatusCode)
	}
	var health HealthResponse
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" {
		t.Fatalf("health status = %q", health.Status)
	}
}

func TestMetadataLanguageSettingsEndpointRequiresConfiguredPreferences(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/settings/metadata-language", nil)
	response := httptest.NewRecorder()
	(StatusHandler{}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("metadata-language settings status = %d, want 503 when preferences are not configured", response.Code)
	}
}

type metadataLanguagePreferenceStub struct {
	language config.MetadataLanguage
	getErr   error
	setErr   error
}

func (settings *metadataLanguagePreferenceStub) GetMetadataLanguage() (config.MetadataLanguage, error) {
	return settings.language, settings.getErr
}

func (settings *metadataLanguagePreferenceStub) SetMetadataLanguage(language config.MetadataLanguage) error {
	if settings.setErr != nil {
		return settings.setErr
	}
	settings.language = language
	return nil
}

func TestMetadataLanguageSettingsEndpointReadsAndPersistsExplicitChoices(t *testing.T) {
	settings := &metadataLanguagePreferenceStub{language: config.MetadataLanguageEnglish}
	router := (StatusHandler{MetadataLanguageSettings: settings}).Routes()

	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, "/api/v1/settings/metadata-language", nil))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"metadata_language":"en"`) {
		t.Fatalf("GET metadata language = %d/%s, want en", getResponse.Code, getResponse.Body.String())
	}

	patchRequest := httptest.NewRequest(http.MethodPatch, "/api/v1/settings/metadata-language", strings.NewReader(`{"metadata_language":"es"}`))
	patchRequest.Header.Set("Content-Type", "application/json")
	patchResponse := httptest.NewRecorder()
	router.ServeHTTP(patchResponse, patchRequest)
	if patchResponse.Code != http.StatusOK || settings.language != config.MetadataLanguageSpanish || !strings.Contains(patchResponse.Body.String(), `"metadata_language":"es"`) {
		t.Fatalf("PATCH metadata language = %d/%s, stored %q", patchResponse.Code, patchResponse.Body.String(), settings.language)
	}
}

func TestMetadataLanguageSettingsEndpointRejectsUnsupportedAndMalformedChoices(t *testing.T) {
	settings := &metadataLanguagePreferenceStub{language: config.MetadataLanguageEnglish}
	router := (StatusHandler{MetadataLanguageSettings: settings}).Routes()
	for _, body := range []string{`{"metadata_language":"fr"}`, `{"metadata_language":"es","extra":true}`, `{`} {
		request := httptest.NewRequest(http.MethodPatch, "/api/v1/settings/metadata-language", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("PATCH body %q status = %d, want 400", body, response.Code)
		}
	}
	if settings.language != config.MetadataLanguageEnglish {
		t.Fatalf("invalid PATCH mutated language to %q", settings.language)
	}
}

func TestSystemStatusReportsAgentOfflineWithoutFailingServer(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := httptest.NewServer((StatusHandler{Database: db, Agent: statusClient{err: context.DeadlineExceeded}}).Routes())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/system/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, agent offline should not fail the endpoint", response.StatusCode)
	}
	var status SystemStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Server.Status != "online" || status.Database.Status != "ready" || status.Agent.Status != "offline" {
		t.Fatalf("system status = %#v", status)
	}
}

func TestSystemStatusReportsMissingUDSAgentAsOffline(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	client := agent.UDSClient{SocketPath: filepath.Join(t.TempDir(), "agent.sock"), Timeout: 100 * time.Millisecond}
	server := httptest.NewServer((StatusHandler{Database: db, Agent: client}).Routes())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/system/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, offline UDS Agent must not fail status", response.StatusCode)
	}
	var status SystemStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Server.Status != "online" || status.Database.Status != "ready" || status.Agent.Status != "offline" {
		t.Fatalf("system status = %#v", status)
	}
}

func TestSystemStatusReportsConnectedAgent(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := httptest.NewServer((StatusHandler{Database: db, Agent: statusClient{status: agent.AgentStatus{State: agent.StateOnline, StartedAt: time.Now()}}}).Routes())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/system/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status SystemStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Agent.Status != "connected" {
		t.Fatalf("agent status = %q", status.Agent.Status)
	}
}

func TestStatusEndpointsRejectNonGET(t *testing.T) {
	server := httptest.NewServer((StatusHandler{}).Routes())
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d", response.StatusCode)
	}
}

func TestSearchEndpointTrimsQueryAndReturnsNormalizedResults(t *testing.T) {
	source := &metadataSourceStub{results: []domain.MetadataSearchResult{{
		Provider: "igdb", ExternalID: "1001", Title: "Soulcalibur II", ReleaseYear: 2002,
		Summary: "Synthetic fixture", CoverReference: "https://example.test/cover.jpg",
		Platforms: []domain.Platform{{Name: "Nintendo GameCube", Slug: "nintendo-gamecube"}},
		Medium:    domain.MediumGame, WorkType: "game",
	}}}
	server := httptest.NewServer((StatusHandler{Metadata: source}).Routes())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/search?q=%20%20Soul%20Calibur%20%20")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content type = %q, want JSON", got)
	}
	if source.query != "Soul Calibur" || source.calls != 1 {
		t.Fatalf("source query/calls = %q/%d, want trimmed query and one call", source.query, source.calls)
	}
	var searchResponse domain.SearchResponse
	if err := json.NewDecoder(response.Body).Decode(&searchResponse); err != nil {
		t.Fatal(err)
	}
	if len(searchResponse.Results) != 1 || searchResponse.Results[0].Provider != "igdb" || searchResponse.Results[0].ExternalID != "1001" || searchResponse.Results[0].Title != "Soulcalibur II" {
		t.Fatalf("normalized response = %#v", searchResponse)
	}
	if searchResponse.Results[0].ReleaseYear != 2002 || searchResponse.Results[0].Summary != "Synthetic fixture" || searchResponse.Results[0].CoverReference != "https://example.test/cover.jpg" ||
		searchResponse.Results[0].Medium != domain.MediumGame || searchResponse.Results[0].WorkType != "game" || len(searchResponse.Results[0].Platforms) != 1 || searchResponse.Results[0].Platforms[0].Slug != "nintendo-gamecube" {
		t.Fatalf("normalized metadata fields = %#v", searchResponse.Results[0])
	}
	if len(searchResponse.Sources) != 1 || searchResponse.Sources[0].Provider != "igdb" || searchResponse.Sources[0].State != domain.SearchSourceAvailable {
		t.Fatalf("source status = %#v, want explicit available provider state", searchResponse.Sources)
	}
}

func TestUniversalSearchEndpointReturnsResultAndSourceEnvelope(t *testing.T) {
	searchSource := &universalSearchSourceStub{response: domain.SearchResponse{
		Results: []domain.MetadataSearchResult{{Provider: "openlibrary", ExternalID: "/works/OL1W", Title: "A Wheel of Time", MediaType: "book", Medium: domain.MediumLiterature, WorkType: "book"}},
		Sources: []domain.SearchSourceStatus{{Provider: "openlibrary", State: domain.SearchSourceAvailable, ResultCount: 1}, {Provider: "tvmaze", State: domain.SearchSourceThrottled, ErrorCode: "provider_throttled"}},
	}}
	server := httptest.NewServer((StatusHandler{Search: searchSource}).Routes())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/search?q=Wheel%20of%20Time&limit=7")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got domain.SearchResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || searchSource.query != "Wheel of Time" || searchSource.limit != 7 {
		t.Fatalf("status/query/limit = %d/%q/%d, want 200/trimmed query/7", response.StatusCode, searchSource.query, searchSource.limit)
	}
	if len(got.Results) != 1 || got.Results[0].Provider != "openlibrary" || len(got.Sources) != 2 || got.Sources[1].State != domain.SearchSourceThrottled {
		t.Fatalf("universal search envelope = %#v", got)
	}
}

func TestSearchEndpointPersistsAutomaticUniverseAndReturnsSeparateConfidence(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "search-discovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	searchSource := &universalSearchSourceStub{response: domain.SearchResponse{
		Results: []domain.MetadataSearchResult{
			{Provider: "igdb", ExternalID: "game-1", Title: "Case Closed: Detective Conan", MediaType: "game", Medium: domain.MediumGame, WorkType: "game", Relevance: domain.SearchRelevanceBest},
			{Provider: "tvmaze", ExternalID: "show-1", Title: "Detective Conan: The Movie", MediaType: "series", Medium: domain.MediumVideo, WorkType: "series", Relevance: domain.SearchRelevanceBest},
			{Provider: "openlibrary", ExternalID: "book-1", Title: "Detective Conan and the Vanished", MediaType: "book", Medium: domain.MediumLiterature, WorkType: "book", Relevance: domain.SearchRelevanceBest},
			{Provider: "openlibrary", ExternalID: "author-collision", Title: "Arthur Conan Doyle's Sherlock Holmes", MediaType: "book", Medium: domain.MediumLiterature, WorkType: "book", Relevance: domain.SearchRelevanceWeak, Creators: []string{"Detective Conan"}, Summary: "A description that mentions Detective Conan."},
		},
		Sources: []domain.SearchSourceStatus{{Provider: "igdb", State: domain.SearchSourceAvailable}},
	}}
	service := catalog.NewUniverseApplication(catalog.NewSQLiteRepository(db))
	server := httptest.NewServer((StatusHandler{Search: searchSource, Universes: service}).Routes())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/search?q=Detective%20Conan")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var searchResponse domain.SearchResponse
	if err := json.NewDecoder(response.Body).Decode(&searchResponse); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || searchResponse.UniverseDiscovery == nil {
		t.Fatalf("search status/discovery = %d/%#v, want 200 with automatic discovery outcome", response.StatusCode, searchResponse.UniverseDiscovery)
	}
	discovery := searchResponse.UniverseDiscovery
	if discovery.State != domain.UniverseDiscoveryCreated || discovery.UniverseID == "" || discovery.MembershipsAdded != 3 || discovery.ConfirmedByUser || discovery.Provenance != domain.UniverseProvenanceAutomatic {
		t.Fatalf("search discovery outcome = %#v, want high-confidence unconfirmed automatic Universe and three Works", discovery)
	}
	if discovery.ExistenceConfidence <= 0.95 {
		t.Fatalf("Universe existence confidence = %v, want higher than individual phrase membership confidence", discovery.ExistenceConfidence)
	}
	if discovery.NamingConfidence <= 0 || discovery.NamingConfidence == discovery.ExistenceConfidence || discovery.NamingEvidence == "" || discovery.Title != "Detective Conan" {
		t.Fatalf("Universe naming decision = %#v, want query name and confidence independent from existence", discovery)
	}
	repeatedResponse, err := http.Get(server.URL + "/api/v1/search?q=Detective%20Conan")
	if err != nil {
		t.Fatal(err)
	}
	defer repeatedResponse.Body.Close()
	var repeatedSearch domain.SearchResponse
	if err := json.NewDecoder(repeatedResponse.Body).Decode(&repeatedSearch); err != nil {
		t.Fatal(err)
	}
	if repeatedResponse.StatusCode != http.StatusOK || repeatedSearch.UniverseDiscovery == nil ||
		repeatedSearch.UniverseDiscovery.State != domain.UniverseDiscoveryReused ||
		repeatedSearch.UniverseDiscovery.UniverseID != discovery.UniverseID ||
		repeatedSearch.UniverseDiscovery.Title != discovery.Title || repeatedSearch.UniverseDiscovery.MembershipsAdded != 0 {
		t.Fatalf("repeated Search discovery = %#v; want idempotent reuse of %q/%q without new memberships", repeatedSearch.UniverseDiscovery, discovery.UniverseID, discovery.Title)
	}

	detailResponse, err := http.Get(server.URL + "/api/v1/universes/" + string(discovery.UniverseID))
	if err != nil {
		t.Fatal(err)
	}
	defer detailResponse.Body.Close()
	var detail struct {
		ExistenceConfidence float64 `json:"existence_confidence"`
		NamingConfidence    float64 `json:"naming_confidence"`
		NamingEvidence      string  `json:"naming_evidence"`
		Provenance          string  `json:"provenance"`
		ConfirmedByUser     bool    `json:"confirmed_by_user"`
		Memberships         []struct {
			Provider        string  `json:"provider"`
			ExternalID      string  `json:"external_id"`
			Title           string  `json:"title"`
			MediaType       string  `json:"media_type"`
			Confidence      float64 `json:"confidence"`
			Evidence        string  `json:"evidence"`
			Reason          string  `json:"reason"`
			Provenance      string  `json:"provenance"`
			ConfirmedByUser bool    `json:"confirmed_by_user"`
		} `json:"memberships"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detailResponse.StatusCode != http.StatusOK || detail.ExistenceConfidence != discovery.ExistenceConfidence ||
		detail.NamingConfidence != discovery.NamingConfidence || detail.NamingEvidence != string(discovery.NamingEvidence) ||
		detail.Provenance != string(domain.UniverseProvenanceAutomatic) || detail.ConfirmedByUser || len(detail.Memberships) != 3 {
		t.Fatalf("persisted Universe detail = %#v (status %d)", detail, detailResponse.StatusCode)
	}
	for _, membership := range detail.Memberships {
		if membership.Provider == "" || membership.ExternalID == "" || membership.Title == "" || membership.MediaType == "" ||
			membership.Confidence <= 0 || membership.Evidence == "" || membership.Reason == "" ||
			membership.Provenance != string(domain.UniverseMembershipProvenanceAutomatic) || membership.ConfirmedByUser {
			t.Errorf("membership identity/media/confidence/evidence/provenance/confirmation = %#v, want complete unconfirmed automatic record", membership)
		}
	}
}

func TestSearchEndpointKeepsWeakUniverseEvidenceOutOfMemberships(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "weak-search-discovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	searchSource := &universalSearchSourceStub{response: domain.SearchResponse{
		Results: []domain.MetadataSearchResult{
			{Provider: "igdb", ExternalID: "weak-game", Title: "Neon Genesis Evangelion", MediaType: "game", Medium: domain.MediumGame, WorkType: "game", Relevance: domain.SearchRelevanceWeak},
			{Provider: "tvmaze", ExternalID: "weak-series", Title: "Evangelion Rebuild", MediaType: "series", Medium: domain.MediumVideo, WorkType: "series", Relevance: domain.SearchRelevanceWeak},
			{Provider: "openlibrary", ExternalID: "weak-book", Title: "End of Evangelion", MediaType: "book", Medium: domain.MediumLiterature, WorkType: "book", Relevance: domain.SearchRelevanceWeak},
		},
	}}
	service := catalog.NewUniverseApplication(catalog.NewSQLiteRepository(db))
	server := httptest.NewServer((StatusHandler{Search: searchSource, Universes: service}).Routes())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/search?q=Evangelion")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var searchResponse domain.SearchResponse
	if err := json.NewDecoder(response.Body).Decode(&searchResponse); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || searchResponse.UniverseDiscovery == nil {
		t.Fatalf("weak Search status/discovery = %d/%#v, want 200 with an existence outcome", response.StatusCode, searchResponse.UniverseDiscovery)
	}
	if searchResponse.UniverseDiscovery.State != domain.UniverseDiscoveryCreated || searchResponse.UniverseDiscovery.Title != "Evangelion" || searchResponse.UniverseDiscovery.MembershipsAdded != 0 {
		t.Fatalf("weak Search discovery = %#v, want query-named Universe and zero memberships", searchResponse.UniverseDiscovery)
	}
	for _, result := range searchResponse.Results {
		if result.Relevance != domain.SearchRelevanceWeak {
			t.Errorf("Search relevance for %q = %q, want Weak", result.Title, result.Relevance)
		}
	}
	var works, memberships int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM universe_memberships").Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if works != 0 || memberships != 0 {
		t.Fatalf("weak Search materialized %d Works and %d memberships, want zero", works, memberships)
	}
}

func TestUniversalSearchEndpointRejectsInvalidLimit(t *testing.T) {
	for _, query := range []string{"0", "21", "nope", "1&limit=2"} {
		t.Run(query, func(t *testing.T) {
			source := &universalSearchSourceStub{}
			server := httptest.NewServer((StatusHandler{Search: source}).Routes())
			defer server.Close()
			response, err := http.Get(server.URL + "/api/v1/search?q=title&limit=" + query)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest || source.calls != 0 {
				t.Fatalf("status/calls = %d/%d, want 400/0", response.StatusCode, source.calls)
			}
		})
	}
}

func TestSearchEndpointRejectsEmptyQuery(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "missing", path: "/api/v1/search"},
		{name: "empty", path: "/api/v1/search?q="},
		{name: "whitespace", path: "/api/v1/search?q=%20%20%20"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &metadataSourceStub{}
			server := httptest.NewServer((StatusHandler{Metadata: source}).Routes())
			defer server.Close()
			response, err := http.Get(server.URL + test.path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status code = %d, want 400", response.StatusCode)
			}
			if source.calls != 0 {
				t.Fatalf("source calls = %d, want 0 for empty query", source.calls)
			}
		})
	}
}

func TestSearchEndpointMapsErrorsToSafeMessages(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantText   string
	}{
		{name: "provider not configured", err: igdb.ErrConfiguration, wantStatus: http.StatusServiceUnavailable, wantText: "not configured"},
		{name: "provider outage", err: igdb.ErrProviderUnavailable, wantStatus: http.StatusServiceUnavailable, wantText: "temporarily unavailable"},
		{name: "provider rate limit", err: igdb.ErrRateLimited, wantStatus: http.StatusServiceUnavailable, wantText: "temporarily unavailable"},
		{name: "invalid provider query", err: igdb.ErrInvalidQuery, wantStatus: http.StatusBadRequest, wantText: "invalid search query"},
		{name: "authentication failure", err: igdb.ErrAuthentication, wantStatus: http.StatusBadGateway, wantText: "authentication"},
		{name: "malformed provider response", err: igdb.ErrMalformedResponse, wantStatus: http.StatusBadGateway, wantText: "invalid response"},
		{name: "unexpected private error", err: errors.New("private provider body synthetic-secret"), wantStatus: http.StatusInternalServerError, wantText: "search failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &metadataSourceStub{err: test.err}
			server := httptest.NewServer((StatusHandler{Metadata: source}).Routes())
			defer server.Close()
			response, err := http.Get(server.URL + "/api/v1/search?q=Soulcalibur")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != test.wantStatus || !strings.Contains(string(body), test.wantText) {
				t.Fatalf("status/body = %d/%q, want %d containing %q", response.StatusCode, body, test.wantStatus, test.wantText)
			}
			if strings.Contains(string(body), test.err.Error()) || strings.Contains(string(body), "synthetic-secret") {
				t.Fatalf("unsafe provider detail leaked in response: %q", body)
			}
		})
	}
}

func TestSearchEndpointRejectsNonGET(t *testing.T) {
	server := httptest.NewServer((StatusHandler{}).Routes())
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/search?q=Soulcalibur", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("method response = %d allow=%q, want 405 with Allow: GET", response.StatusCode, response.Header.Get("Allow"))
	}
}

func TestSearchEndpointUsesSQLiteCacheAcrossEquivalentQueries(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	source := &metadataSourceStub{results: []domain.MetadataSearchResult{{
		Provider: "igdb", ExternalID: "1001", Title: "Soulcalibur II", Medium: domain.MediumGame, WorkType: "game",
	}}}
	cached := searchcache.New(source, db, "igdb")
	server := httptest.NewServer((StatusHandler{Metadata: cached}).Routes())
	defer server.Close()

	for _, query := range []string{"%20Soul%20Calibur%20", "soul%20calibur"} {
		response, err := http.Get(server.URL + "/api/v1/search?q=" + query)
		if err != nil {
			t.Fatal(err)
		}
		var searchResponse domain.SearchResponse
		if err := json.NewDecoder(response.Body).Decode(&searchResponse); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK || len(searchResponse.Results) != 1 {
			t.Fatalf("response = %d/%#v, want 200 with one result", response.StatusCode, searchResponse)
		}
	}
	if source.calls != 1 {
		t.Fatalf("metadata source calls = %d, want one fresh lookup", source.calls)
	}
}

func TestInventoryResolutionEndpointValidatesRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"work_identity":`},
		{name: "missing identity", body: `{"work":{"title":"Synthetic","medium":"game","work_type":"game"},"edition":{"platform":"platform-x","format":"disc-image"}}`},
		{name: "missing edition field", body: `{"work_identity":{"provider":"test-catalog","external_id":"record-42"},"work":{"title":"Synthetic","medium":"game","work_type":"game"},"edition":{"platform":"platform-x"}}`},
		{name: "invalid medium", body: `{"work_identity":{"provider":"test-catalog","external_id":"record-42"},"work":{"title":"Synthetic","medium":"unknown","work_type":"game"},"edition":{"platform":"platform-x","format":"disc-image"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			(StatusHandler{}).Routes().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d/body=%q, want 400", response.Code, response.Body.String())
			}
		})
	}
}

func TestInventoryResolutionUsesTrustedWorkDetailsForFirstBind(t *testing.T) {
	tests := []struct {
		name       string
		details    domain.WorkDetails
		detailsErr error
		wantStatus int
	}{
		{
			name: "exact identity binds normalized provider details",
			details: domain.WorkDetails{
				Provider: "test-catalog", ExternalID: "record-42", Title: "Trusted Normalized Title",
				Medium: domain.MediumGame, WorkType: "canonical-game",
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "details error leaves graph unbound",
			detailsErr: errors.New("synthetic details provider failure"),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "different canonical provider identity leaves graph unbound",
			details: domain.WorkDetails{
				Provider: "other-catalog", ExternalID: "record-42", Title: "Wrong Identity",
				Medium: domain.MediumGame, WorkType: "game",
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			repository := catalog.NewSQLiteRepository(db)
			source := inventory.NewManifestInventorySource(writeSyntheticInventoryManifest(t))
			details := &workDetailsSourceStub{details: test.details, err: test.detailsErr}
			binder := catalog.NewInventoryBinder(repository, source, source)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(`{"work_identity":{"provider":"test-catalog","external_id":"record-42"},"edition":{"platform":"platform-x","format":"disc-image"}}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			(StatusHandler{Details: details, Inventory: binder}).Routes().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("inventory response = %d/%q, want status %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if details.calls != 1 || details.provider != "test-catalog" || details.externalID != "record-42" {
				t.Fatalf("details lookup = calls:%d identity:%s:%s, want exactly test-catalog:record-42", details.calls, details.provider, details.externalID)
			}
			graph, lookupErr := repository.GetWorkByExternalIdentity(request.Context(), "test-catalog", "record-42")
			if test.wantStatus == http.StatusOK {
				if lookupErr != nil {
					t.Fatalf("lookup trusted Work: %v", lookupErr)
				}
				if graph.Work.Title != "Trusted Normalized Title" || graph.Work.Medium != domain.MediumGame || graph.Work.WorkType != "canonical-game" {
					t.Fatalf("persisted Work = %#v, want only trusted normalized details", graph.Work)
				}
				return
			}
			if !errors.Is(lookupErr, catalog.ErrNotFound) {
				t.Fatalf("failed detail lookup left graph %#v (error %v); want no Work graph", graph, lookupErr)
			}
			assertCatalogEmpty(t, db)
		})
	}
}

func TestInventoryResolutionRejectsCallerSuppliedCanonicalWorkMetadata(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := catalog.NewSQLiteRepository(db)
	source := inventory.NewManifestInventorySource(writeSyntheticInventoryManifest(t))
	details := &workDetailsSourceStub{details: domain.WorkDetails{
		Provider: "test-catalog", ExternalID: "record-42", Title: "Trusted Normalized Title",
		Medium: domain.MediumGame, WorkType: "canonical-game",
	}}
	body := `{"work_identity":{"provider":"test-catalog","external_id":"record-42"},"work":{"title":"Forged Caller Title","medium":"literature","work_type":"book"},"edition":{"platform":"platform-x","format":"disc-image"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Details: details, Inventory: catalog.NewInventoryBinder(repository, source, source)}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("forged metadata response = %d/%q, want 400", response.Code, response.Body.String())
	}
	if details.calls != 0 {
		t.Fatalf("forged request called WorkDetails %d times, want no resolution", details.calls)
	}
	if _, err := repository.GetWorkByExternalIdentity(request.Context(), "test-catalog", "record-42"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("forged request created or exposed a canonical graph: %v", err)
	}
	assertCatalogEmpty(t, db)
}

func assertCatalogEmpty(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"works", "external_identities", "editions", "assets", "asset_parts", "asset_locations"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count catalog table %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("catalog table %s has %d rows, want none", table, count)
		}
	}
}

func TestInventoryResolutionEndpointRequiresConfiguredManifest(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(syntheticInventoryResolveRequest))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{}).Routes().ServeHTTP(response, request)
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || result["code"] != "inventory_not_configured" || !strings.Contains(response.Body.String(), "not configured") {
		t.Fatalf("status/body = %d/%q, want safe 503 for missing config", response.Code, response.Body.String())
	}
}

func TestInventoryResolutionEndpointReportsMissingManifestAsUnconfigured(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	source := inventory.NewManifestInventorySource(filepath.Join(t.TempDir(), "missing-manifest.json"))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(syntheticInventoryResolveRequest))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Inventory: catalog.NewInventoryBinder(catalog.NewSQLiteRepository(db), source, source)}).Routes().ServeHTTP(response, request)
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || result["code"] != "inventory_not_configured" {
		t.Fatalf("status/body = %d/%q, want safe missing-Manifest state", response.Code, response.Body.String())
	}
}

func TestInventoryResolutionEndpointPersistsExactMatchAndOmitsManifestDetails(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manifestPath := writeSyntheticInventoryManifest(t)
	source := inventory.NewManifestInventorySource(manifestPath)
	details := &workDetailsSourceStub{details: domain.WorkDetails{
		Provider: "test-catalog", ExternalID: "record-42", Title: "Trusted Synthetic Work",
		Medium: domain.MediumGame, WorkType: "canonical-game",
	}}
	binder := catalog.NewInventoryBinder(catalog.NewSQLiteRepository(db), source, source)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(syntheticInventoryResolveRequest))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Details: details, Inventory: binder}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%q, want 200", response.Code, response.Body.String())
	}
	var result struct {
		Owned        bool     `json:"owned"`
		Availability string   `json:"availability"`
		WorkID       string   `json:"work_id"`
		EditionID    string   `json:"edition_id"`
		AssetIDs     []string `json:"asset_ids"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Owned || result.Availability != "archived" || result.WorkID == "" || result.EditionID == "" || len(result.AssetIDs) != 1 || result.AssetIDs[0] != "synthetic-asset-42" {
		t.Fatalf("resolution result = %#v, want owned archived canonical graph", result)
	}
	for _, private := range []string{"synthetic-private-locator", "Manifest Private Display", manifestPath} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("response leaked manifest detail %q: %s", private, response.Body.String())
		}
	}
}

func TestInventoryResolutionEndpointLoadsCheckedInSoulcaliburFixture(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manifestPath := filepath.Join("..", "..", "examples", "manifest.json")
	source := inventory.NewManifestInventorySource(manifestPath)
	binder := catalog.NewInventoryBinder(catalog.NewSQLiteRepository(db), source, source)
	details := &workDetailsSourceStub{details: domain.WorkDetails{
		Provider: "igdb", ExternalID: "1565", Title: "Soulcalibur II",
		Medium: domain.MediumGame, WorkType: "game",
	}}
	requestBody := `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Details: details, Inventory: binder}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("checked-in fixture response = %d/%q, want 200", response.Code, response.Body.String())
	}
	var result InventoryResolveResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Owned || result.Availability != catalog.AvailabilityArchived || result.WorkID == "" ||
		string(result.WorkID) == "1565" || result.EditionID == "" || len(result.AssetIDs) != 1 ||
		result.AssetIDs[0] != "soulcalibur2-gamecube" {
		t.Fatalf("checked-in fixture API result = %#v, want owned Archived canonical graph", result)
	}
}

func TestInventoryResolutionEndpointNoMatchDoesNotCreateCatalogRows(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := catalog.NewSQLiteRepository(db)
	source := inventory.NewManifestInventorySource(writeSyntheticInventoryManifest(t))
	requestBody := strings.Replace(syntheticInventoryResolveRequest, `"record-42"`, `"record-404"`, 1)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Inventory: catalog.NewInventoryBinder(repository, source, source)}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%q, want 200 unavailable result", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["owned"] != false || result["availability"] != "unavailable" {
		t.Fatalf("no-match result = %#v, want not owned/unavailable", result)
	}
	if _, exists := result["work_id"]; exists {
		t.Fatalf("no-match response exposed canonical IDs: %#v", result)
	}
	if _, err := repository.GetWorkByExternalIdentity(request.Context(), "test-catalog", "record-404"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("no-match catalog lookup error = %v, want ErrNotFound", err)
	}
}

func TestInventoryResolutionEndpointMapsManifestFailuresToSafeError(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manifestPath := filepath.Join(t.TempDir(), "private-manifest-path", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const privatePayload = `{"private":"synthetic-private-manifest-payload"}`
	if err := os.WriteFile(manifestPath, []byte(privatePayload), 0o600); err != nil {
		t.Fatal(err)
	}
	source := inventory.NewManifestInventorySource(manifestPath)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(syntheticInventoryResolveRequest))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Inventory: catalog.NewInventoryBinder(catalog.NewSQLiteRepository(db), source, source)}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "temporarily unavailable") {
		t.Fatalf("status/body = %d/%q, want safe 503", response.Code, response.Body.String())
	}
	for _, private := range []string{manifestPath, privatePayload, "synthetic-private-manifest-payload"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("response leaked manifest failure detail %q: %s", private, response.Body.String())
		}
	}
}

func TestInventoryResolutionEndpointRejectsNonPOST(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/inventory/resolve", nil)
	response := httptest.NewRecorder()
	(StatusHandler{}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method response = %d allow=%q, want 405 with Allow: POST", response.Code, response.Header().Get("Allow"))
	}
}

func TestWorkDetailsEndpointReturnsProviderNeutralCanonicalDetails(t *testing.T) {
	source := &workDetailsSourceStub{details: domain.WorkDetails{
		Provider: "igdb", ExternalID: "1565", Title: "Soulcalibur II", ReleaseYear: 2002,
		Summary: "Canonical Work summary.", Medium: domain.MediumGame, WorkType: "game",
		PlatformCandidates: []domain.PlatformCandidate{{
			Platform:   domain.Platform{Name: "Nintendo GameCube", Slug: "nintendo-gamecube"},
			Provenance: []domain.PlatformProvenance{{Provider: "igdb", ExternalID: "227987", Relation: domain.PlatformRelationParentGameChild}},
		}},
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider=igdb&external_id=227987", nil)
	response := httptest.NewRecorder()
	(StatusHandler{Details: source}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("details response = %d/%q, want 200", response.Code, response.Body.String())
	}
	var details domain.WorkDetails
	if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
		t.Fatal(err)
	}
	if details.Provider != "igdb" || details.ExternalID != "1565" || details.Title != "Soulcalibur II" || len(details.PlatformCandidates) != 1 {
		t.Fatalf("normalized details = %#v", details)
	}
	if source.provider != "igdb" || source.externalID != "227987" || source.calls != 1 {
		t.Fatalf("details source called with %q/%q %d times, want igdb/227987 once", source.provider, source.externalID, source.calls)
	}
	for _, privateField := range []string{"parent_game", "access_token", "client_secret", "raw_igdb"} {
		if strings.Contains(response.Body.String(), privateField) {
			t.Fatalf("normalized details exposed provider-only field %q: %s", privateField, response.Body.String())
		}
	}
}

func TestWorkDetailsEndpointEnrichesOnlyExactSeriesMembership(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "work-details-series.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := catalog.NewSQLiteRepository(db)
	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "exact-work", Title: "A Similar Author Book", Medium: domain.MediumLiterature, WorkType: "book",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "similar-title-work", Title: "A Similar Author Book", Medium: domain.MediumLiterature, WorkType: "book",
	}); err != nil {
		t.Fatal(err)
	}
	collection := domain.WorkCollection{ID: "opaque-series-identity", Title: "Separate Series Label", Type: domain.WorkCollectionTypeSeries}
	if err := repository.CreateWorkCollection(ctx, collection); err != nil {
		t.Fatal(err)
	}
	ordinal := "1"
	if _, err := repository.SetWorkRelation(ctx, domain.WorkRelation{
		WorkID: work.Work.ID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID,
		Ordinal: &ordinal, Provenance: domain.WorkRelationProvenanceManual, Confidence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewUniverseApplication(repository)
	requestDetails := func(externalID string) domain.WorkDetails {
		t.Helper()
		source := &workDetailsSourceStub{details: domain.WorkDetails{
			Provider: "fixture", ExternalID: externalID, Title: "A Similar Author Book",
			Medium: domain.MediumLiterature, WorkType: "book", PlatformCandidates: []domain.PlatformCandidate{},
		}}
		handler := (StatusHandler{Details: source, Universes: service}).Routes()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider=fixture&external_id="+externalID, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("work-details %q response = %d/%s", externalID, response.Code, response.Body.String())
		}
		var details domain.WorkDetails
		if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
			t.Fatal(err)
		}
		if source.calls != 1 || source.provider != "fixture" || source.externalID != externalID {
			t.Fatalf("provider source identity = %q/%q calls=%d", source.provider, source.externalID, source.calls)
		}
		return details
	}

	exact := requestDetails("exact-work")
	if len(exact.Series) != 1 || exact.Series[0].CollectionID != collection.ID || exact.Series[0].Title != collection.Title ||
		exact.Series[0].Ordinal == nil || *exact.Series[0].Ordinal != ordinal || exact.Series[0].KnownTotal == nil || *exact.Series[0].KnownTotal != 1 ||
		len(exact.Series[0].MoreInSeries) != 0 {
		t.Fatalf("exact Work series details = %#v", exact.Series)
	}
	if exact.Series[0].MoreInSeries == nil {
		t.Fatalf("known series has nil peer list; want an explicit empty list: %#v", exact.Series[0])
	}
	similar := requestDetails("similar-title-work")
	if len(similar.Series) != 0 {
		t.Fatalf("title-similar Work inherited another Work's series identity: %#v", similar.Series)
	}
}

func TestWorkDetailsEndpointValidatesMethodAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		url    string
		status int
	}{
		{name: "rejects post", method: http.MethodPost, url: "/api/v1/work-details?provider=igdb&external_id=1565", status: http.StatusMethodNotAllowed},
		{name: "missing provider", method: http.MethodGet, url: "/api/v1/work-details?external_id=1565", status: http.StatusBadRequest},
		{name: "missing external id", method: http.MethodGet, url: "/api/v1/work-details?provider=igdb", status: http.StatusBadRequest},
		{name: "duplicate provider", method: http.MethodGet, url: "/api/v1/work-details?provider=igdb&provider=igdb&external_id=1565", status: http.StatusBadRequest},
		{name: "oversized identity", method: http.MethodGet, url: "/api/v1/work-details?provider=igdb&external_id=" + strings.Repeat("1", 513), status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &workDetailsSourceStub{}
			request := httptest.NewRequest(test.method, test.url, nil)
			response := httptest.NewRecorder()
			(StatusHandler{Details: source}).Routes().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("response = %d/%q, want %d", response.Code, response.Body.String(), test.status)
			}
			if source.calls != 0 {
				t.Fatalf("invalid request invoked provider %d times, want zero", source.calls)
			}
			if test.method == http.MethodPost && response.Header().Get("Allow") != http.MethodGet {
				t.Fatalf("Allow header = %q, want GET", response.Header().Get("Allow"))
			}
		})
	}
}

func TestWorkDetailsEndpointMapsProviderFailuresToSafeErrors(t *testing.T) {
	const privateFailure = "private oauth response and client-secret value"
	source := &workDetailsSourceStub{err: errors.New(privateFailure)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider=igdb&external_id=1565", nil)
	response := httptest.NewRecorder()
	(StatusHandler{Details: source}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "temporarily unavailable") {
		t.Fatalf("failure response = %d/%q, want safe 502", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), privateFailure) || strings.Contains(response.Body.String(), "oauth") {
		t.Fatalf("provider error details leaked: %s", response.Body.String())
	}
}

func TestWorkDetailsEndpointMapsProviderNeutralFailuresWithoutLeakingDetails(t *testing.T) {
	const privateFailure = "private provider body and credential"
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid external ID", err: providers.ErrDetailsInvalidExternalID, status: http.StatusBadRequest},
		{name: "unsupported provider", err: providers.ErrDetailsUnsupportedProvider, status: http.StatusBadRequest},
		{name: "not found", err: providers.ErrDetailsWorkNotFound, status: http.StatusNotFound},
		{name: "rate limited", err: providers.ErrDetailsRateLimited, status: http.StatusServiceUnavailable},
		{name: "unavailable", err: providers.ErrDetailsProviderUnavailable, status: http.StatusServiceUnavailable},
		{name: "malformed response", err: providers.ErrDetailsMalformedResponse, status: http.StatusBadGateway},
		{name: "oversized response", err: providers.ErrDetailsResponseTooLarge, status: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &workDetailsSourceStub{err: errors.Join(test.err, errors.New(privateFailure))}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider=openlibrary&external_id=OL1W", nil)
			response := httptest.NewRecorder()
			(StatusHandler{Details: source}).Routes().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("failure response = %d/%q, want %d", response.Code, response.Body.String(), test.status)
			}
			if strings.Contains(response.Body.String(), privateFailure) || strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("provider details leaked in public response: %s", response.Body.String())
			}
		})
	}
}

func TestInventoryResolutionKeepsCanonicalExternalIdentitySeparateFromOpaqueWorkID(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manifestPath := filepath.Join(t.TempDir(), "canonical-inventory.json")
	const manifest = `{"schema_version":1,"assets":[{"id":"soulcalibur2-gamecube","work":{"title":"Soulcalibur II","external_ids":{"igdb":"1565"}},"edition":{"platform":"gamecube","format":"disc_image"},"total_size_bytes":4,"parts":[{"role":"rom","filename":"Soulcalibur II.iso","size_bytes":4}],"locations":[{"class":"archive","provider":"synthetic-storage","locator":"synthetic-private-locator"}]}]}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	source := inventory.NewManifestInventorySource(manifestPath)
	details := &workDetailsSourceStub{details: domain.WorkDetails{
		Provider: "igdb", ExternalID: "1565", Title: "Soulcalibur II",
		Medium: domain.MediumGame, WorkType: "game",
	}}
	requestBody := `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(StatusHandler{Details: details, Inventory: catalog.NewInventoryBinder(catalog.NewSQLiteRepository(db), source, source)}).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("canonical inventory response = %d/%q, want 200", response.Code, response.Body.String())
	}
	var result InventoryResolveResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Owned || result.Availability != catalog.AvailabilityArchived || result.WorkID == "" || string(result.WorkID) == "1565" || len(result.AssetIDs) != 1 || result.AssetIDs[0] != "soulcalibur2-gamecube" {
		t.Fatalf("inventory result = %#v, want exact canonical identity with opaque local Work ID", result)
	}
	graph, err := catalog.NewSQLiteRepository(db).GetWorkByExternalIdentity(request.Context(), "igdb", "1565")
	if err != nil {
		t.Fatal(err)
	}
	if graph.Work.ID != result.WorkID {
		t.Fatalf("persisted Work ID = %q, response Work ID = %q", graph.Work.ID, result.WorkID)
	}
}

const syntheticInventoryResolveRequest = `{"work_identity":{"provider":"test-catalog","external_id":"record-42"},"edition":{"platform":"platform-x","format":"disc-image"}}`

func writeSyntheticInventoryManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	const manifest = `{"schema_version":1,"assets":[{"id":"synthetic-asset-42","work":{"title":"Manifest Private Display","external_ids":{"test-catalog":"record-42"}},"edition":{"platform":"platform-x","format":"disc-image"},"total_size_bytes":4,"parts":[{"role":"data","filename":"synthetic.bin","size_bytes":4}],"locations":[{"class":"archive","provider":"synthetic-storage","locator":"synthetic-private-locator"}]}]}`
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type metadataSourceStub struct {
	calls   int
	query   string
	results []domain.MetadataSearchResult
	err     error
}

func TestSearchKeepsProviderResultsWhenOptionalUniversePersistenceFails(t *testing.T) {
	for _, mode := range []string{"universal-search", "metadata-search"} {
		t.Run(mode, func(t *testing.T) {
			db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "discovery-failure.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			service := catalog.NewUniverseApplication(catalog.NewSQLiteRepository(db))
			results := []domain.MetadataSearchResult{
				{Provider: "igdb", ExternalID: "game-1", Title: "Case Closed: Detective Conan", MediaType: "game", Medium: domain.MediumGame, WorkType: "game"},
				{Provider: "tvmaze", ExternalID: "show-1", Title: "Detective Conan: The Movie", MediaType: "series", Medium: domain.MediumVideo, WorkType: "series"},
				{Provider: "openlibrary", ExternalID: "book-1", Title: "Detective Conan and the Vanished", MediaType: "book", Medium: domain.MediumLiterature, WorkType: "book"},
			}
			var logs bytes.Buffer
			handler := StatusHandler{
				Universes: service,
				Logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			}
			if mode == "universal-search" {
				handler.Search = &universalSearchSourceStub{response: domain.SearchResponse{
					Results: results,
					Sources: []domain.SearchSourceStatus{{Provider: "igdb", State: domain.SearchSourceAvailable, ResultCount: len(results)}},
				}}
			} else {
				handler.Metadata = &metadataSourceStub{results: results}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Detective%20Conan", nil)
			response := httptest.NewRecorder()
			handler.Routes().ServeHTTP(response, request)
			var got domain.SearchResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode Search response %q: %v", response.Body.String(), err)
			}
			if response.Code != http.StatusOK || len(got.Results) != len(results) || got.UniverseDiscovery != nil {
				t.Fatalf("Search after discovery persistence failure = %d/%#v; want 200 with all provider results and no uncertain Universe result", response.Code, got)
			}
			logOutput := logs.String()
			if !strings.Contains(logOutput, "category=catalog_unavailable") || strings.Contains(logOutput, "Detective Conan") || strings.Contains(logOutput, "database is closed") {
				t.Fatalf("discovery failure log = %q; want safe category only", logOutput)
			}
		})
	}
}

func TestSearchProviderFailureStillReturnsErrorBeforeUniverseDiscovery(t *testing.T) {
	searchSource := &universalSearchSourceStub{err: errors.New("synthetic-secret provider failure")}
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "provider-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var logs bytes.Buffer
	service := catalog.NewUniverseApplication(catalog.NewSQLiteRepository(db))
	handler := StatusHandler{
		Search: searchSource, Universes: service,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Detective%20Conan", nil)
	response := httptest.NewRecorder()
	handler.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "synthetic-secret") || logs.Len() != 0 {
		t.Fatalf("provider failure response/log = %d/%q/%q; want safe error and no optional discovery warning", response.Code, response.Body.String(), logs.String())
	}
}

func TestUniverseRelationshipResolutionRequiresExplicitExactAnchorNotSearchCards(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "relationship-resolve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := catalog.NewSQLiteRepository(db)
	if err := repository.CreateUniverse(context.Background(), domain.Universe{ID: "got-explicit", Title: "Game of Thrones"}); err != nil {
		t.Fatal(err)
	}
	source := &relationshipAPIMetadataStub{candidate: wikidata.EntityCandidate{ID: "Q800", Label: "Game of Thrones", Language: "en"}, metadata: providers.RelationshipMetadata{
		Provider: "wikidata", ExternalID: "Q800", EntityRevision: "12", ParserVersion: "wikidata-relationship-v3", FetchedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		Aliases: []providers.RelationshipLocalizedValue{{Language: "es", Value: "Juego de Tronos"}},
	}}
	resolver := catalog.NewUniverseRelationshipResolver(repository, source, catalog.NewSQLiteRelationshipMetadataCache(db))
	service := catalog.NewUniverseApplicationWithRelationshipResolver(repository, resolver)
	search := &universalSearchSourceStub{response: domain.SearchResponse{Results: []domain.MetadataSearchResult{{Provider: "openlibrary", ExternalID: "OL800W", Title: "Game of Thrones", Medium: domain.MediumLiterature, WorkType: "novel"}}}}
	router := (StatusHandler{Universes: service, Search: search}).Routes()
	searchResponse := httptest.NewRecorder()
	router.ServeHTTP(searchResponse, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Game%20of%20Thrones", nil))
	if searchResponse.Code != http.StatusOK || source.searches != 0 || len(source.fetches) != 0 {
		t.Fatalf("ordinary Search triggered provider expansion: status=%d searches=%d fetches=%v", searchResponse.Code, source.searches, source.fetches)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/universes/relationships/resolve", strings.NewReader(`{"alias":"Game of Thrones","language":"en"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || source.searches != 1 || len(source.fetches) != 1 {
		t.Fatalf("explicit Universe relationship request = %d/%q; provider search/fetch=%d/%v", response.Code, response.Body.String(), source.searches, source.fetches)
	}
	graph, err := repository.GetUniverse(context.Background(), "got-explicit")
	if err != nil || len(graph.Aliases) != 1 || graph.Aliases[0].Alias != "Juego de Tronos" {
		t.Fatalf("localized alias was not added to existing Universe: graph=%#v error=%v", graph, err)
	}
}

type relationshipAPIMetadataStub struct {
	candidate wikidata.EntityCandidate
	metadata  providers.RelationshipMetadata
	searches  int
	fetches   []string
}

func (stub *relationshipAPIMetadataStub) SearchEntities(_ context.Context, _, _ string, _ int) ([]wikidata.EntityCandidate, error) {
	stub.searches++
	return []wikidata.EntityCandidate{stub.candidate}, nil
}

func (stub *relationshipAPIMetadataStub) FetchEntity(_ context.Context, qid string, _ []string) (providers.RelationshipMetadata, error) {
	stub.fetches = append(stub.fetches, qid)
	return stub.metadata, nil
}

type universalSearchSourceStub struct {
	calls    int
	query    string
	limit    int
	response domain.SearchResponse
	err      error
}

func (source *universalSearchSourceStub) Search(_ context.Context, query string, limit int) (domain.SearchResponse, error) {
	source.calls++
	source.query = query
	source.limit = limit
	return source.response, source.err
}

type workDetailsSourceStub struct {
	calls      int
	provider   string
	externalID string
	details    domain.WorkDetails
	err        error
}

func (source *workDetailsSourceStub) GetWorkDetails(_ context.Context, provider, externalID string) (domain.WorkDetails, error) {
	source.calls++
	source.provider = provider
	source.externalID = externalID
	return source.details, source.err
}

func (source *metadataSourceStub) SearchWorks(_ context.Context, query string) ([]domain.MetadataSearchResult, error) {
	source.calls++
	source.query = query
	return source.results, source.err
}
