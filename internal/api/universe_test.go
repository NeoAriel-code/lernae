package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/catalog"
	"lernae/internal/database"
	"lernae/internal/domain"
)

type universeTestServer struct {
	db     *sql.DB
	path   string
	server *httptest.Server
}

func newUniverseTestServer(t *testing.T) universeTestServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "universe.db")
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := catalog.NewUniverseApplication(catalog.NewSQLiteRepository(db))
	server := httptest.NewServer((StatusHandler{Database: db, Universes: service}).Routes())
	t.Cleanup(server.Close)
	return universeTestServer{db: db, path: path, server: server}
}

func (server universeTestServer) request(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, server.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, data
}

type universeAPIResponse struct {
	ID                   string                       `json:"id"`
	Title                string                       `json:"title"`
	ExistenceConfidence  float64                      `json:"existence_confidence"`
	Provenance           string                       `json:"provenance"`
	ConfirmedByUser      bool                         `json:"confirmed_by_user"`
	Universes            []universeSummaryResponse    `json:"universes"`
	Memberships          []universeMembershipResponse `json:"memberships"`
	Suggestions          []universeMembershipResponse `json:"suggestions"`
	Exclusions           []universeExclusionResponse  `json:"exclusions"`
	MembershipsTruncated bool                         `json:"memberships_truncated"`
	ExclusionsTruncated  bool                         `json:"exclusions_truncated"`
	Candidates           []universeCandidateResponse  `json:"candidates"`
}

type universeSummaryResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type universeMembershipResponse struct {
	Provider        string  `json:"provider"`
	ExternalID      string  `json:"external_id"`
	Title           string  `json:"title"`
	Medium          string  `json:"medium"`
	WorkType        string  `json:"work_type"`
	Provenance      string  `json:"provenance"`
	Confidence      float64 `json:"confidence"`
	Evidence        string  `json:"evidence"`
	Reason          string  `json:"reason"`
	ConfirmedByUser bool    `json:"confirmed_by_user"`
}

type universeExclusionResponse struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason"`
}

type universeCandidateResponse struct {
	ID          string                     `json:"id"`
	Title       string                     `json:"title"`
	Existing    bool                       `json:"existing"`
	Evidence    string                     `json:"evidence"`
	Reason      string                     `json:"reason"`
	Confidence  float64                    `json:"confidence"`
	Memberships []universeProposalResponse `json:"memberships"`
}

type universeProposalResponse struct {
	UniverseTitle   string  `json:"universe_title"`
	Provider        string  `json:"provider"`
	ExternalID      string  `json:"external_id"`
	Title           string  `json:"title"`
	ExistingState   string  `json:"existing_state"`
	Evidence        string  `json:"evidence"`
	ConfirmedByUser bool    `json:"confirmed_by_user"`
	Suppressed      bool    `json:"suppressed"`
	Reason          string  `json:"reason"`
	Confidence      float64 `json:"confidence"`
}

func decodeUniverseResponse(t *testing.T, body []byte) universeAPIResponse {
	t.Helper()
	var response universeAPIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode Universe API response %s: %v", body, err)
	}
	return response
}

func TestConfirmAutomaticUniverseEndpointConfirmsOnlyExplicitGroupAndPreservesEvidence(t *testing.T) {
	server := newUniverseTestServer(t)
	application := catalog.NewUniverseApplication(catalog.NewSQLiteRepository(server.db))
	results := []domain.MetadataSearchResult{
		{Provider: "igdb", ExternalID: "conan-game", Title: "Detective Conan Game", Medium: domain.MediumGame, WorkType: "game", Relevance: domain.SearchRelevanceBest},
		{Provider: "tvmaze", ExternalID: "conan-series", Title: "Detective Conan Series", Medium: domain.MediumVideo, WorkType: "series", Relevance: domain.SearchRelevanceBest},
		{Provider: "openlibrary", ExternalID: "conan-book", Title: "Detective Conan Novel", Medium: domain.MediumLiterature, WorkType: "book", Relevance: domain.SearchRelevanceBest},
	}
	discovery, err := application.AutoDiscover(context.Background(), "Detective Conan", results)
	if err != nil || discovery.State != domain.UniverseDiscoveryCreated || discovery.ConfirmedByUser {
		t.Fatalf("automatic discovery = %#v, %v; want unconfirmed created Universe", discovery, err)
	}

	response, body := server.request(t, http.MethodPost, "/api/v1/universes/"+string(discovery.UniverseID)+"/confirm", "")
	got := decodeUniverseResponse(t, body)
	if response.StatusCode != http.StatusOK || got.ID != string(discovery.UniverseID) || !got.ConfirmedByUser ||
		got.Provenance != string(domain.UniverseProvenanceAutomatic) || got.ExistenceConfidence != discovery.ExistenceConfidence {
		t.Fatalf("explicit group confirmation = %d/%#v, want confirmed automatic Universe with unchanged confidence", response.StatusCode, got)
	}
	if len(got.Memberships) != len(results) {
		t.Fatalf("confirmed Universe memberships = %#v, want %d", got.Memberships, len(results))
	}
	for _, membership := range got.Memberships {
		if membership.Provenance != string(domain.UniverseMembershipProvenanceAutomatic) || membership.ConfirmedByUser {
			t.Errorf("group confirmation changed Work confirmation semantics: %#v", membership)
		}
	}

	replay, replayBody := server.request(t, http.MethodPost, "/api/v1/universes/"+string(discovery.UniverseID)+"/confirm", "")
	replayed := decodeUniverseResponse(t, replayBody)
	if replay.StatusCode != http.StatusOK || !replayed.ConfirmedByUser || replayed.Provenance != got.Provenance || replayed.ExistenceConfidence != got.ExistenceConfidence {
		t.Fatalf("idempotent group confirmation = %d/%#v", replay.StatusCode, replayed)
	}
}

func TestUniverseDetailExposesLegacyUnknownAutomaticMembershipAsSuggestion(t *testing.T) {
	server := newUniverseTestServer(t)
	const id = "legacy-review-universe"
	if _, err := server.db.Exec(`INSERT INTO universes (id, title) VALUES (?, 'Evangelion')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`INSERT INTO works (id, medium, work_type, title, summary) VALUES ('legacy-weak-work', 'game', 'game', 'Neon Genesis Evangelion', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`INSERT INTO external_identities (id, work_id, provider, external_id) VALUES ('legacy-identity', 'legacy-weak-work', 'igdb', '3555')`); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, evidence, reason, accepted_at_utc, confirmed_by_user, status)
		VALUES ('legacy-weak-work', ?, 'automatic', 0.9, 'unknown', 'No supported matching evidence was found.', '2026-09-30T12:00:00Z', 0, 'review_needed')`, id); err != nil {
		t.Fatal(err)
	}

	response, body := server.request(t, http.MethodGet, "/api/v1/universes/"+id, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Universe detail status = %d: %s", response.StatusCode, body)
	}
	got := decodeUniverseResponse(t, body)
	if len(got.Memberships) != 0 || len(got.Suggestions) != 1 {
		t.Fatalf("active/suggested detail = %d/%d; want no active membership and one review suggestion", len(got.Memberships), len(got.Suggestions))
	}
	suggestion := got.Suggestions[0]
	if suggestion.Provider != "igdb" || suggestion.ExternalID != "3555" || suggestion.Provenance != string(domain.UniverseMembershipProvenanceAutomatic) ||
		suggestion.Confidence != 0.9 || suggestion.Evidence != "unknown" || suggestion.Reason != "No supported matching evidence was found." || suggestion.ConfirmedByUser {
		t.Fatalf("review suggestion = %#v, want exact legacy identity and preserved unconfirmed provenance/evidence", suggestion)
	}
	var rows int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM universe_memberships WHERE work_id = 'legacy-weak-work'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("persisted legacy membership rows = %d, want retained row", rows)
	}
	confirm := `{"provider":"igdb","external_id":"3555","medium":"game","work_type":"game","title":"Neon Genesis Evangelion"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+id+"/memberships", confirm)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("explicit review confirmation status = %d: %s", response.StatusCode, body)
	}
	confirmed := decodeUniverseResponse(t, body)
	if len(confirmed.Memberships) != 1 || len(confirmed.Suggestions) != 0 {
		t.Fatalf("explicit confirmation state = memberships %d/suggestions %d; want accepted relation", len(confirmed.Memberships), len(confirmed.Suggestions))
	}
	reset := `{"action":"clear_confirmation","provider":"igdb","external_id":"3555"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+id+"/reset", reset)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("clear explicit confirmation status = %d: %s", response.StatusCode, body)
	}
	resetState := decodeUniverseResponse(t, body)
	if len(resetState.Memberships) != 0 || len(resetState.Suggestions) != 1 {
		t.Fatalf("reset state = memberships %d/suggestions %d; want original evidence back in review", len(resetState.Memberships), len(resetState.Suggestions))
	}
}

func TestUniverseAPIBoundsSearchGetResolveAndExactIdentity(t *testing.T) {
	server := newUniverseTestServer(t)
	invalidBodies := []string{
		`{"operation_id":"bad","title":"Universe","works":[],"unknown":true}`,
		`{"operation_id":"bad","title":"Universe","works":[]} {}`,
	}
	for _, body := range invalidBodies {
		response, _ := server.request(t, http.MethodPost, "/api/v1/universes", body)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("create with malformed/unknown JSON status = %d, want 400", response.StatusCode)
		}
	}
	tooLarge := `{"operation_id":"bad","title":"` + strings.Repeat("x", 70<<10) + `","works":[]}`
	response, _ := server.request(t, http.MethodPost, "/api/v1/universes", tooLarge)
	if response.StatusCode != http.StatusRequestEntityTooLarge && response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized create status = %d, want bounded 400/413", response.StatusCode)
	}
	response, _ = server.request(t, http.MethodPost, "/api/v1/universes/some-id", "{}")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("search method status = %d, want 405", response.StatusCode)
	}

	createBody := `{"operation_id":"create-shared-story-1","title":"Shared Story","works":[` +
		`{"provider":"openlibrary","external_id":"/works/OL-1W","medium":"literature","work_type":"book","title":"Shared Story"},` +
		`{"provider":"tvmaze","external_id":"1001","medium":"video","work_type":"series","title":"Shared Story"}]}`
	response, body := server.request(t, http.MethodPost, "/api/v1/universes", createBody)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", response.StatusCode, body)
	}
	created := decodeUniverseResponse(t, body)
	if created.ID == "" || created.Title != "Shared Story" || len(created.Memberships) != 2 {
		t.Fatalf("created Universe = %#v, want two exact selected Works", created)
	}
	for _, membership := range created.Memberships {
		if membership.Provenance != "manual" || !membership.ConfirmedByUser || membership.Confidence != 1 {
			t.Errorf("server confirmation fields = %#v, want manual confirmed membership", membership)
		}
	}
	if created.Memberships[0].Provider == created.Memberships[1].Provider && created.Memberships[0].ExternalID == created.Memberships[1].ExternalID {
		t.Fatalf("distinct providers were conflated: %#v", created.Memberships)
	}

	response, body = server.request(t, http.MethodPost, "/api/v1/universes", createBody)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("idempotent create retry status = %d, want 200: %s", response.StatusCode, body)
	}
	retried := decodeUniverseResponse(t, body)
	if retried.ID != created.ID {
		t.Fatalf("idempotent create retry ID = %q, want original %q", retried.ID, created.ID)
	}

	response, body = server.request(t, http.MethodGet, "/api/v1/universes?q=Shared%20Story&limit=10", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Universe search status = %d: %s", response.StatusCode, body)
	}
	search := decodeUniverseResponse(t, body)
	if len(search.Universes) != 1 || search.Universes[0].ID != created.ID {
		t.Fatalf("Universe search response = %#v", search)
	}

	response, body = server.request(t, http.MethodGet, "/api/v1/universes/"+created.ID, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Universe get status = %d: %s", response.StatusCode, body)
	}
	got := decodeUniverseResponse(t, body)
	if len(got.Memberships) != 2 || got.Memberships[0].Title != "Shared Story" {
		t.Fatalf("Universe details = %#v", got)
	}

	resolveBody := `{"query":"Shared Story","results":[` +
		`{"provider":"openlibrary","external_id":"/works/OL-1W","medium":"literature","work_type":"book","title":"Shared Story"},` +
		`{"provider":"catalog","external_id":"different-id","medium":"video","work_type":"series","title":"Shared Story"}]}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/resolve", resolveBody)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d: %s", response.StatusCode, body)
	}
	proposals := decodeUniverseResponse(t, body)
	if len(proposals.Candidates) != 1 || !proposals.Candidates[0].Existing || proposals.Candidates[0].ID != created.ID {
		t.Fatalf("resolve candidates = %#v", proposals.Candidates)
	}
	var confirmed, proposed bool
	for _, membership := range proposals.Candidates[0].Memberships {
		if membership.UniverseTitle != created.Title {
			t.Errorf("resolved existing Universe title = %q, want stored title %q", membership.UniverseTitle, created.Title)
		}
		switch membership.ExternalID {
		case "/works/OL-1W":
			confirmed = membership.ConfirmedByUser && membership.ExistingState == "accepted"
		case "different-id":
			proposed = membership.ExistingState == "none" && !membership.ConfirmedByUser
		}
	}
	if !confirmed || !proposed {
		t.Fatalf("resolver did not distinguish confirmed identity and new same-title identity: %#v", proposals.Candidates[0].Memberships)
	}
	if _, err := server.db.Exec(`INSERT INTO universe_aliases (universe_id, alias, provenance, created_at_utc) VALUES (?, 'Shared Saga', 'manual', '2000-01-01T00:00:00Z')`, created.ID); err != nil {
		t.Fatal(err)
	}
	aliasResolve := `{"query":"Shared Saga","results":[{"provider":"catalog","external_id":"alias-anchor","medium":"video","work_type":"series","title":"Shared Saga Book"}]}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/resolve", aliasResolve)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("alias resolve status = %d: %s", response.StatusCode, body)
	}
	aliasProposal := decodeUniverseResponse(t, body)
	if len(aliasProposal.Candidates) != 1 || aliasProposal.Candidates[0].ID != created.ID ||
		len(aliasProposal.Candidates[0].Memberships) != 1 || aliasProposal.Candidates[0].Memberships[0].Evidence != "explicit_alias" {
		t.Fatalf("resolver did not use the stored explicit alias: %#v", aliasProposal.Candidates)
	}
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/resolve", `{"query":"Shared Story","results":[{"provider":"x","external_id":"1","medium":"video","work_type":"series","title":"Shared Story","artwork_url":"https://unsafe.invalid"}]}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("resolve with arbitrary display URL status = %d, want 400: %s", response.StatusCode, body)
	}
	var workCount int
	if err := server.db.QueryRow("SELECT COUNT(*) FROM works").Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if workCount != 2 {
		t.Fatalf("proposal resolution materialized Work rows: count=%d, want 2", workCount)
	}
	var confirmedMemberships int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM universe_memberships WHERE confirmed_by_user = 1 AND provenance = 'manual'`).Scan(&confirmedMemberships); err != nil {
		t.Fatal(err)
	}
	if confirmedMemberships != 2 {
		t.Fatalf("proposal rerun changed confirmed membership state: count=%d, want 2", confirmedMemberships)
	}
}

func TestUniverseResolveNewCandidateHasBoundedProposedDisplayTitle(t *testing.T) {
	server := newUniverseTestServer(t)
	query := "  Evangelion  "
	resolve := `{"query":"` + query + `","results":[` +
		`{"provider":"igdb","external_id":"game-1","medium":"game","work_type":"game","title":"Neon Genesis Evangelion"},` +
		`{"provider":"openlibrary","external_id":"book-1","medium":"literature","work_type":"book","title":"The End of Evangelion"},` +
		`{"provider":"tvmaze","external_id":"show-1","medium":"video","work_type":"series","title":"Evangelion: Rebuild"}]}`
	response, body := server.request(t, http.MethodPost, "/api/v1/universes/resolve", resolve)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve new candidate status = %d: %s", response.StatusCode, body)
	}
	proposals := decodeUniverseResponse(t, body)
	if len(proposals.Candidates) != 1 || proposals.Candidates[0].Existing {
		t.Fatalf("new query-anchored candidates = %#v, want one non-existing candidate", proposals.Candidates)
	}
	if len(proposals.Candidates[0].Memberships) != 3 {
		t.Fatalf("new query-anchored proposals = %#v, want three related memberships", proposals.Candidates[0].Memberships)
	}
	wantTitle := strings.TrimSpace(query)
	for _, membership := range proposals.Candidates[0].Memberships {
		if membership.UniverseTitle == "" || len(membership.UniverseTitle) > 200 {
			t.Errorf("new proposal Universe title = %q, want a non-empty title bounded to 200 characters", membership.UniverseTitle)
		}
		if membership.UniverseTitle != wantTitle {
			t.Errorf("new proposal Universe title = %q, want trimmed query display title %q", membership.UniverseTitle, wantTitle)
		}
	}
}

func TestUniverseAPICorrectionsPersistAndResetOnlyRequestedDecision(t *testing.T) {
	server := newUniverseTestServer(t)
	create := `{"operation_id":"create-correction-test","title":"The Example Series","works":[{"provider":"tvmaze","external_id":"accepted","medium":"video","work_type":"series","title":"The Example Series"}]}`
	response, body := server.request(t, http.MethodPost, "/api/v1/universes", create)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.StatusCode, body)
	}
	created := decodeUniverseResponse(t, body)

	confirm := `{"provider":"catalog-x","external_id":"new-confirmed","medium":"literature","work_type":"book","title":"The Example Series"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/memberships", confirm)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("confirm Work status = %d: %s", response.StatusCode, body)
	}
	confirmAgain := confirm
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/memberships", confirmAgain)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("repeat exact identity confirmation status = %d: %s", response.StatusCode, body)
	}
	var workCount, membershipCount int
	if err := server.db.QueryRow("SELECT COUNT(*) FROM works").Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow("SELECT COUNT(*) FROM universe_memberships").Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if workCount != 2 || membershipCount != 2 {
		t.Fatalf("exact identity retry duplicated rows: works=%d memberships=%d, want 2/2", workCount, membershipCount)
	}
	differentID := `{"provider":"catalog-x","external_id":"new-confirmed-2","medium":"literature","work_type":"book","title":"The Example Series"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/memberships", differentID)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("same-title different exact ID status = %d: %s", response.StatusCode, body)
	}
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/memberships", differentID)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("same-title identity retry status = %d: %s", response.StatusCode, body)
	}
	if err := server.db.QueryRow("SELECT COUNT(*) FROM works").Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow("SELECT COUNT(*) FROM universe_memberships").Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if workCount != 3 || membershipCount != 3 {
		t.Fatalf("same-provider distinct IDs were conflated or retry duplicated: works=%d memberships=%d, want 3/3", workCount, membershipCount)
	}
	ruleConfirmation := `{"provider":"catalog-z","external_id":"rule-confirmed","medium":"video","work_type":"series","title":"The Example Series"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/memberships", ruleConfirmation)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("add rule-backed confirmation fixture status = %d: %s", response.StatusCode, body)
	}
	var ruleWorkID string
	if err := server.db.QueryRow(`SELECT work_id FROM external_identities WHERE provider = 'catalog-z' AND external_id = 'rule-confirmed'`).Scan(&ruleWorkID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`UPDATE universe_memberships SET provenance = 'rule' WHERE work_id = ?`, ruleWorkID); err != nil {
		t.Fatal(err)
	}

	reject := `{"work":{"provider":"catalog-y","external_id":"rejected","medium":"video","work_type":"series","title":"The Example Series"},"reason":"not the same story"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/reject", reject)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reject unmaterialized identity status = %d: %s", response.StatusCode, body)
	}
	resolve := `{"query":"The Example Series","results":[{"provider":"catalog-y","external_id":"rejected","medium":"video","work_type":"series","title":"The Example Series"}]}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/resolve", resolve)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve rejected identity status = %d: %s", response.StatusCode, body)
	}
	rejectedProposal := decodeUniverseResponse(t, body)
	if len(rejectedProposal.Candidates) != 1 || len(rejectedProposal.Candidates[0].Memberships) != 1 || !rejectedProposal.Candidates[0].Memberships[0].Suppressed {
		t.Fatalf("rejected identity was not suppressed: %#v", rejectedProposal.Candidates)
	}

	remove := `{"provider":"catalog-x","external_id":"new-confirmed","reason":"manual removal"}`
	response, body = server.request(t, http.MethodDelete, "/api/v1/universes/"+created.ID+"/memberships", remove)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("remove membership status = %d: %s", response.StatusCode, body)
	}
	resolveRemoved := `{"query":"The Example Series","results":[{"provider":"catalog-x","external_id":"new-confirmed","medium":"literature","work_type":"book","title":"The Example Series"}]}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/resolve", resolveRemoved)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve removed identity status = %d: %s", response.StatusCode, body)
	}
	removedProposal := decodeUniverseResponse(t, body)
	if len(removedProposal.Candidates) != 1 || !removedProposal.Candidates[0].Memberships[0].Suppressed {
		t.Fatalf("removed identity was not suppressed by exclusion: %#v", removedProposal.Candidates)
	}

	resetExclusion := `{"action":"clear_exclusion","provider":"catalog-x","external_id":"new-confirmed"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/reset", resetExclusion)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reset exclusion status = %d: %s", response.StatusCode, body)
	}
	var exclusions int
	if err := server.db.QueryRow("SELECT COUNT(*) FROM universe_exclusions WHERE universe_id = ?", created.ID).Scan(&exclusions); err != nil {
		t.Fatal(err)
	}
	if exclusions != 1 {
		t.Fatalf("reset exclusion touched another identity's decision: remaining exclusions=%d, want rejected identity only", exclusions)
	}

	resetConfirmation := `{"action":"clear_confirmation","provider":"tvmaze","external_id":"accepted"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/reset", resetConfirmation)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reset confirmation status = %d: %s", response.StatusCode, body)
	}
	resetRuleConfirmation := `{"action":"clear_confirmation","provider":"catalog-z","external_id":"rule-confirmed"}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes/"+created.ID+"/reset", resetRuleConfirmation)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reset rule-backed confirmation status = %d: %s", response.StatusCode, body)
	}
	response, body = server.request(t, http.MethodGet, "/api/v1/universes/"+created.ID, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get after reset status = %d: %s", response.StatusCode, body)
	}
	got := decodeUniverseResponse(t, body)
	if len(got.Memberships) != 2 {
		t.Fatalf("reset confirmation changed unrelated membership: %#v", got.Memberships)
	}
	var keptManual, keptRule bool
	for _, membership := range got.Memberships {
		if membership.ExternalID == "new-confirmed-2" && membership.ConfirmedByUser && membership.Provenance == "manual" {
			keptManual = true
		}
		if membership.ExternalID == "rule-confirmed" && !membership.ConfirmedByUser && membership.Provenance == "rule" {
			keptRule = true
		}
	}
	if !keptManual || !keptRule {
		t.Fatalf("reset confirmation changed non-target decision or erased non-manual evidence: %#v", got.Memberships)
	}
	if len(got.Exclusions) != 1 || got.Exclusions[0].ExternalID != "rejected" {
		t.Fatalf("reset of confirmation/exclusion changed unrelated rejection: %#v", got.Exclusions)
	}
}

func TestUniverseAPICreateAndConfirmAreAtomic(t *testing.T) {
	server := newUniverseTestServer(t)
	if _, err := server.db.Exec(`CREATE TRIGGER reject_universe_membership BEFORE INSERT ON universe_memberships BEGIN SELECT RAISE(ABORT, 'fixture rejects membership'); END`); err != nil {
		t.Fatal(err)
	}
	body := `{"operation_id":"atomic-failure","title":"Must Roll Back","works":[{"provider":"tvmaze","external_id":"atomic","medium":"video","work_type":"series","title":"Must Roll Back"}]}`
	response, responseBody := server.request(t, http.MethodPost, "/api/v1/universes", body)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("create with injected membership failure status = %d, want 500: %s", response.StatusCode, responseBody)
	}
	for _, table := range []string{"universes", "works", "external_identities", "universe_memberships", "universe_create_operations"} {
		var count int
		if err := server.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("failed atomic create left %d rows in %s", count, table)
		}
	}
}

func TestUniverseAPICreateGetAndSearchExposeNullableMetadataAndTimestamps(t *testing.T) {
	server := newUniverseTestServer(t)
	body := `{"operation_id":"metadata-operation","title":"Display Title","sort_title":"Alternate Sort","description":"Plain text description","works":[{"provider":"fixture","external_id":"metadata-work","medium":"video","work_type":"series","title":"Display Title"}]}`
	response, responseBody := server.request(t, http.MethodPost, "/api/v1/universes", body)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create with supported metadata status = %d, want 201: %s", response.StatusCode, responseBody)
	}
	created := decodeJSONMap(t, responseBody)
	if err := assertUniverseMetadataJSON(t, created); err != nil {
		t.Fatal(err)
	}
	if created["sort_title"] != "Alternate Sort" || created["description"] != "Plain text description" || created["artwork"] != nil {
		t.Fatalf("created metadata = %#v, want supported text and null artwork", created)
	}
	id, ok := created["id"].(string)
	if !ok || id == "" {
		t.Fatalf("created Universe ID = %#v, want non-empty string", created["id"])
	}
	createdAt := created["created_at"].(string)
	updatedAt := created["updated_at"].(string)
	if createdAt != updatedAt {
		t.Fatalf("created timestamps = %q/%q, want creation timestamps to match", createdAt, updatedAt)
	}

	response, responseBody = server.request(t, http.MethodGet, "/api/v1/universes/"+id, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get metadata Universe status = %d: %s", response.StatusCode, responseBody)
	}
	detail := decodeJSONMap(t, responseBody)
	if err := assertUniverseMetadataJSON(t, detail); err != nil {
		t.Fatal(err)
	}
	if detail["sort_title"] != "Alternate Sort" || detail["description"] != "Plain text description" || detail["artwork"] != nil || detail["created_at"] != createdAt || detail["updated_at"] != updatedAt {
		t.Fatalf("Universe detail metadata = %#v, want create values preserved", detail)
	}

	response, responseBody = server.request(t, http.MethodGet, "/api/v1/universes?q=Display%20Title", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search metadata Universe status = %d: %s", response.StatusCode, responseBody)
	}
	search := decodeJSONMap(t, responseBody)
	universeRows, ok := search["universes"].([]any)
	if !ok || len(universeRows) != 1 {
		t.Fatalf("Universe search response = %#v, want one row", search)
	}
	summary, ok := universeRows[0].(map[string]any)
	if !ok || summary["id"] != id {
		t.Fatalf("Universe search summary = %#v, want ID %q", universeRows[0], id)
	}
	if err := assertUniverseMetadataJSON(t, summary); err != nil {
		t.Fatal(err)
	}
	if summary["sort_title"] != "Alternate Sort" || summary["description"] != "Plain text description" || summary["artwork"] != nil || summary["created_at"] != createdAt || summary["updated_at"] != updatedAt {
		t.Fatalf("Universe search metadata = %#v, want create values preserved", summary)
	}

	response, responseBody = server.request(t, http.MethodPost, "/api/v1/universes", body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("idempotent metadata replay status = %d, want 200: %s", response.StatusCode, responseBody)
	}
	replay := decodeJSONMap(t, responseBody)
	if replay["id"] != id || replay["created_at"] != createdAt || replay["updated_at"] != updatedAt {
		t.Fatalf("idempotent replay changed Universe identity/timestamps: %#v", replay)
	}
}

func TestUniverseAPICreateDigestIncludesSupportedMetadataAndRejectsArtworkInput(t *testing.T) {
	server := newUniverseTestServer(t)
	first := `{"operation_id":"metadata-digest","title":"Display Title","sort_title":"Sort A","description":"First description","works":[{"provider":"fixture","external_id":"metadata-digest-work","medium":"video","work_type":"series","title":"Display Title"}]}`
	response, firstBody := server.request(t, http.MethodPost, "/api/v1/universes", first)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("initial metadata create status = %d: %s", response.StatusCode, firstBody)
	}
	var changedBody map[string]any
	if err := json.Unmarshal([]byte(first), &changedBody); err != nil {
		t.Fatal(err)
	}
	changedBody["description"] = "Different description"
	encoded, err := json.Marshal(changedBody)
	if err != nil {
		t.Fatal(err)
	}
	response, responseBody := server.request(t, http.MethodPost, "/api/v1/universes", string(encoded))
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("reused operation with changed metadata status = %d, want 409: %s", response.StatusCode, responseBody)
	}

	var withArtwork map[string]any
	if err := json.Unmarshal([]byte(first), &withArtwork); err != nil {
		t.Fatal(err)
	}
	withArtwork["operation_id"] = "artwork-input"
	withArtwork["artwork"] = "https://untrusted.example/artwork.jpg"
	encoded, err = json.Marshal(withArtwork)
	if err != nil {
		t.Fatal(err)
	}
	response, responseBody = server.request(t, http.MethodPost, "/api/v1/universes", string(encoded))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("client-supplied artwork status = %d, want rejected 400: %s", response.StatusCode, responseBody)
	}
	tooLong := strings.Repeat("x", 2049)
	if err := json.Unmarshal([]byte(first), &withArtwork); err != nil {
		t.Fatal(err)
	}
	withArtwork["operation_id"] = "too-long-description"
	withArtwork["description"] = tooLong
	encoded, err = json.Marshal(withArtwork)
	if err != nil {
		t.Fatal(err)
	}
	response, responseBody = server.request(t, http.MethodPost, "/api/v1/universes", string(encoded))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-bound description status = %d, want 400: %s", response.StatusCode, responseBody)
	}
}

func decodeJSONMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode JSON object %s: %v", body, err)
	}
	return decoded
}

func assertUniverseMetadataJSON(t *testing.T, value map[string]any) error {
	t.Helper()
	for _, key := range []string{"sort_title", "description", "artwork", "created_at", "updated_at"} {
		if _, ok := value[key]; !ok {
			return fmt.Errorf("Universe JSON omitted %q: %#v", key, value)
		}
	}
	for _, key := range []string{"created_at", "updated_at"} {
		text, ok := value[key].(string)
		if !ok {
			return fmt.Errorf("Universe %s = %#v, want timestamp string", key, value[key])
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || parsed.IsZero() {
			return fmt.Errorf("Universe %s = %q, want RFC3339Nano timestamp: %v", key, text, err)
		}
	}
	return nil
}

func TestUniverseAPIGetBoundsMembershipList(t *testing.T) {
	server := newUniverseTestServer(t)
	create := `{"operation_id":"bounded-details","title":"Bounded Details","works":[{"provider":"fixture","external_id":"anchor","medium":"video","work_type":"series","title":"Bounded Details"}]}`
	response, body := server.request(t, http.MethodPost, "/api/v1/universes", create)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.StatusCode, body)
	}
	id := decodeUniverseResponse(t, body).ID
	for index := range 101 {
		workID := fmt.Sprintf("fixture-work-%03d", index)
		if _, err := server.db.Exec(`INSERT INTO works (id, medium, work_type, title, summary) VALUES (?, 'video', 'series', ?, '')`, workID, fmt.Sprintf("Fixture %03d", index)); err != nil {
			t.Fatal(err)
		}
		if _, err := server.db.Exec(`INSERT INTO universe_memberships (work_id, universe_id, provenance, confidence, accepted_at_utc, confirmed_by_user) VALUES (?, ?, 'rule', 0.5, '2000-01-01T00:00:00Z', 0)`, workID, id); err != nil {
			t.Fatal(err)
		}
	}
	response, body = server.request(t, http.MethodGet, "/api/v1/universes/"+id, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("bounded get status = %d: %s", response.StatusCode, body)
	}
	got := decodeUniverseResponse(t, body)
	if len(got.Memberships) != 100 || !got.MembershipsTruncated {
		t.Fatalf("bounded Universe memberships = %d, truncated=%t; want 100 and true", len(got.Memberships), got.MembershipsTruncated)
	}
}

func TestUniverseAPIGetExposesPersistedLocalizedAliasesOnSameOpaqueID(t *testing.T) {
	server := newUniverseTestServer(t)
	response, body := server.request(t, http.MethodPost, "/api/v1/universes", `{"operation_id":"localized-aliases","title":"The Wheel of Time","works":[{"provider":"fixture","external_id":"wheel-anchor","medium":"video","work_type":"series","title":"The Wheel of Time"}]}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.StatusCode, body)
	}
	id := decodeUniverseResponse(t, body).ID
	repository := catalog.NewSQLiteRepository(server.db)
	spanish := "es"
	if _, err := repository.AddUniverseAlias(context.Background(), domain.UniverseAlias{
		UniverseID: domain.UniverseID(id), Alias: "La rueda del tiempo", Language: &spanish,
		Provenance: string(domain.UniverseMembershipProvenanceProvider), Confidence: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AddUniverseAlias(context.Background(), domain.UniverseAlias{
		UniverseID: domain.UniverseID(id), Alias: "Alternate name without language",
		Provenance: string(domain.UniverseMembershipProvenanceManual), Confidence: 1,
	}); err != nil {
		t.Fatal(err)
	}

	response, body = server.request(t, http.MethodGet, "/api/v1/universes/"+id, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET Universe status = %d: %s", response.StatusCode, body)
	}
	var details struct {
		ID      string `json:"id"`
		Aliases []struct {
			Value           string  `json:"value"`
			Language        *string `json:"language"`
			Provenance      string  `json:"provenance"`
			ConfirmedByUser bool    `json:"confirmed_by_user"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(body, &details); err != nil {
		t.Fatal(err)
	}
	if details.ID != id || len(details.Aliases) != 2 {
		t.Fatalf("Universe identity/aliases = %q/%#v, want original opaque ID and both persisted aliases", details.ID, details.Aliases)
	}
	if details.Aliases[0].Value != "Alternate name without language" || details.Aliases[0].Language != nil ||
		details.Aliases[0].Provenance != "manual" || !details.Aliases[0].ConfirmedByUser {
		t.Fatalf("nullable-language manual alias = %#v", details.Aliases[0])
	}
	if details.Aliases[1].Value != "La rueda del tiempo" || details.Aliases[1].Language == nil || *details.Aliases[1].Language != spanish ||
		details.Aliases[1].Provenance != "provider" || details.Aliases[1].ConfirmedByUser {
		t.Fatalf("localized provider alias = %#v", details.Aliases[1])
	}
}

func TestUniverseAPICreateOperationIDCannotBeReusedForAnotherRequest(t *testing.T) {
	server := newUniverseTestServer(t)
	first := `{"operation_id":"one-operation","title":"First","works":[{"provider":"openlibrary","external_id":"first","medium":"literature","work_type":"book","title":"First"}]}`
	response, body := server.request(t, http.MethodPost, "/api/v1/universes", first)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("initial create status = %d: %s", response.StatusCode, body)
	}
	original := decodeUniverseResponse(t, body)
	server.server.Close()
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, err := database.Open(context.Background(), server.path)
	if err != nil {
		t.Fatalf("reopen Universe API database: %v", err)
	}
	reopenedServer := httptest.NewServer((StatusHandler{
		Database: reopenedDB, Universes: catalog.NewUniverseApplication(catalog.NewSQLiteRepository(reopenedDB)),
	}).Routes())
	t.Cleanup(func() { reopenedServer.Close(); _ = reopenedDB.Close() })
	server = universeTestServer{db: reopenedDB, path: server.path, server: reopenedServer}
	response, body = server.request(t, http.MethodPost, "/api/v1/universes", first)
	if response.StatusCode != http.StatusOK || decodeUniverseResponse(t, body).ID != original.ID {
		t.Fatalf("create replay after restart status/ID = %d/%q, want 200/%q: %s", response.StatusCode, decodeUniverseResponse(t, body).ID, original.ID, body)
	}
	second := `{"operation_id":"one-operation","title":"Second","works":[{"provider":"openlibrary","external_id":"second","medium":"literature","work_type":"book","title":"Second"}]}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes", second)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("operation ID reuse status = %d, want 409: %s", response.StatusCode, body)
	}
	var universes, works int
	if err := server.db.QueryRow("SELECT COUNT(*) FROM universes").Scan(&universes); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow("SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if universes != 1 || works != 1 {
		t.Fatalf("conflicting operation changed state: universes=%d works=%d", universes, works)
	}

	sameTitle := `{"operation_id":"another-operation","title":"First","works":[{"provider":"openlibrary","external_id":"different","medium":"literature","work_type":"book","title":"First"}]}`
	response, body = server.request(t, http.MethodPost, "/api/v1/universes", sameTitle)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("same-title independent create status = %d, want 201: %s", response.StatusCode, body)
	}
	createdAgain := decodeUniverseResponse(t, body)
	if createdAgain.ID == original.ID {
		t.Fatalf("distinct opaque create operation reused title identity %q", createdAgain.ID)
	}
	response, body = server.request(t, http.MethodGet, "/api/v1/universes?q=First&limit=20", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search same-title Universes status = %d: %s", response.StatusCode, body)
	}
	search := decodeUniverseResponse(t, body)
	if len(search.Universes) != 2 {
		t.Fatalf("same-title local Universes were deduplicated: %#v", search.Universes)
	}
	if err := server.db.QueryRow("SELECT COUNT(*) FROM universes").Scan(&universes); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow("SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if universes != 2 || works != 2 {
		t.Fatalf("same-title independent create was deduplicated: universes=%d works=%d", universes, works)
	}
}
