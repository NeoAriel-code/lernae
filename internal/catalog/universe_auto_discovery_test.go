package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/internal/search"
)

func TestAutoDiscoverUsesSearchAggregatorRelevanceForExistenceButNotMembership(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		titles      []string
		wantCreated bool
	}{
		{
			name:        "evangelion weak results support a named empty universe",
			query:       "Evangelion",
			titles:      []string{"Neon Genesis Evangelion", "Evangelion Rebuild", "End of Evangelion"},
			wantCreated: true,
		},
		{
			name:   "generic game remains fail closed",
			query:  "game",
			titles: []string{"Game", "Video Game", "Game Night"},
		},
		{
			name:   "ambiguous conan remains fail closed",
			query:  "conan",
			titles: []string{"Detective Conan", "Conan the Barbarian", "Conan Exiles"},
		},
	}
	providersByName := []string{"igdb", "tvmaze", "openlibrary"}
	mediaByProvider := []domain.Medium{domain.MediumGame, domain.MediumVideo, domain.MediumLiterature}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			_, application := newAutoDiscoveryApplication(t, "aggregator-"+test.query+".db")
			searchProviders := make([]providers.SearchProvider, 0, len(providersByName))
			for index, providerName := range providersByName {
				searchProviders = append(searchProviders, autoDiscoverySearchProvider{
					name: providerName,
					results: []domain.MetadataSearchResult{
						autoDiscoveryResult(providerName, "work-"+providerName, test.titles[index], mediaByProvider[index]),
					},
				})
			}
			aggregated, err := search.NewAggregator(search.Config{}, searchProviders...).Search(ctx, test.query, 20)
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if len(aggregated.Results) != 3 {
				t.Fatalf("Search() returned %d results, want 3: %#v", len(aggregated.Results), aggregated.Results)
			}
			if test.wantCreated {
				for _, result := range aggregated.Results {
					if result.Relevance != domain.SearchRelevanceWeak {
						t.Errorf("aggregated relevance for %q = %q, want Weak", result.Title, result.Relevance)
					}
				}
			}
			discovery, err := application.AutoDiscover(ctx, test.query, aggregated.Results)
			if err != nil {
				t.Fatalf("AutoDiscover() error = %v", err)
			}
			if !test.wantCreated {
				if discovery.State != domain.UniverseDiscoveryNotEligible || discovery.UniverseID != "" {
					t.Fatalf("AutoDiscover() = %#v, want fail-closed result", discovery)
				}
				var universes int
				if err := application.repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universes); err != nil {
					t.Fatal(err)
				}
				if universes != 0 {
					t.Fatalf("fail-closed query created %d Universes", universes)
				}
				return
			}
			if discovery.State != domain.UniverseDiscoveryCreated || discovery.Title != "Evangelion" || discovery.UniverseID == "" || discovery.MembershipsAdded != 0 {
				t.Fatalf("AutoDiscover() = %#v, want a query-named Evangelion Universe with zero Weak memberships", discovery)
			}
			detail, err := application.GetUniverse(ctx, discovery.UniverseID)
			if err != nil {
				t.Fatalf("GetUniverse() error = %v", err)
			}
			if detail.Universe.Title != "Evangelion" || len(detail.Memberships) != 0 {
				t.Fatalf("persisted Universe = %q with %d memberships; want Evangelion with none", detail.Universe.Title, len(detail.Memberships))
			}
			var works int
			if err := application.repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM works").Scan(&works); err != nil {
				t.Fatal(err)
			}
			if works != 0 {
				t.Fatalf("Weak results materialized %d Works", works)
			}
		})
	}
}

func TestAutoDiscoverWeakEvidenceCannotBypassOtherUniverseOwnership(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "weak-owned-evidence.db")
	repository := application.repository
	owner := domain.Universe{ID: "existing-watchlist", Title: "Existing Watchlist"}
	if err := repository.CreateUniverse(ctx, owner); err != nil {
		t.Fatal(err)
	}
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "weak-game", "Neon Genesis Evangelion", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "weak-series", "Evangelion Rebuild", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "weak-book", "End of Evangelion", domain.MediumLiterature),
	}
	workIDs := make(map[string]domain.WorkID, len(results))
	for index := range results {
		results[index].Relevance = domain.SearchRelevanceWeak
		work, err := repository.MaterializeExternalWork(ctx, resultMaterialization(results[index]))
		if err != nil {
			t.Fatal(err)
		}
		workIDs[results[index].ExternalID] = work.Work.ID
		if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
			UniverseID: owner.ID, WorkID: work.Work.ID, Provenance: domain.UniverseMembershipProvenanceManual,
			Confidence: 1, ConfirmedByUser: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	discovery, err := application.AutoDiscover(ctx, "Evangelion", results)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.State != domain.UniverseDiscoveryNotEligible || discovery.UniverseID != "" {
		t.Fatalf("all-Weak already-owned discovery = %#v, want no duplicate Universe", discovery)
	}
	var universes, memberships int
	if err := repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universes); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universe_memberships").Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if universes != 1 || memberships != len(results) {
		t.Fatalf("ownership-filtered catalog counts = universes %d, memberships %d; want 1/%d", universes, memberships, len(results))
	}
	for _, result := range results {
		membership, err := repository.GetWorkUniverseMembership(ctx, workIDs[result.ExternalID])
		if err != nil || membership.UniverseID != owner.ID || membership.Provenance != domain.UniverseMembershipProvenanceManual || !membership.ConfirmedByUser {
			t.Errorf("existing exact identity %s/%s changed: %#v, %v", result.Provider, result.ExternalID, membership, err)
		}
	}
}

func TestAutoDiscoverReviewSuggestionStillBlocksCrossUniverseOwnershipEvidence(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "review-owned-evidence.db")
	owner := domain.Universe{ID: "existing-watchlist", Title: "Existing Watchlist"}
	if err := application.repository.CreateUniverse(ctx, owner); err != nil {
		t.Fatal(err)
	}
	ownedResult := autoDiscoveryResult("igdb", "weak-owned", "Neon Genesis Evangelion", domain.MediumGame)
	owned, err := application.repository.MaterializeExternalWork(ctx, resultMaterialization(ownedResult))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, evidence, reason, accepted_at_utc, confirmed_by_user, status)
		VALUES (?, ?, 'automatic', 0.9, 'unknown', 'No supported matching evidence was found.', '2026-09-30T12:00:00Z', 0, 'review_needed')`, owned.Work.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	results := []domain.MetadataSearchResult{
		ownedResult,
		autoDiscoveryResult("tvmaze", "weak-new-series", "Evangelion Rebuild", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "weak-new-book", "End of Evangelion", domain.MediumLiterature),
	}
	for index := range results {
		results[index].Relevance = domain.SearchRelevanceWeak
	}
	discovery, err := application.AutoDiscover(ctx, "Evangelion", results)
	if err != nil {
		t.Fatalf("AutoDiscover() error = %v", err)
	}
	if discovery.State != domain.UniverseDiscoveryNotEligible || discovery.UniverseID != "" {
		t.Fatalf("discovery from review-owned evidence = %#v, want fail-closed without a duplicate Universe", discovery)
	}
	var universeCount, membershipCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universes`).Scan(&universeCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universe_memberships WHERE work_id = ? AND universe_id = ?`, owned.Work.ID, owner.ID).Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if universeCount != 1 || membershipCount != 1 {
		t.Fatalf("review-owned state changed: universes=%d owner rows=%d, want 1/1", universeCount, membershipCount)
	}
}

func TestAutoDiscoverWeakEvidenceRespectsTargetExclusions(t *testing.T) {
	tests := []struct {
		name             string
		excludeOneResult bool
		wantState        domain.UniverseDiscoveryState
	}{
		{name: "unexcluded Weak cohort remains usable", wantState: domain.UniverseDiscoveryReused},
		{name: "excluded Weak identity cannot complete threshold", excludeOneResult: true, wantState: domain.UniverseDiscoveryNotEligible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			_, application := newAutoDiscoveryApplication(t, "weak-exclusion-"+test.name+".db")
			repository := application.repository
			target := domain.Universe{ID: "evangelion", Title: "Evangelion"}
			if err := repository.CreateUniverse(ctx, target); err != nil {
				t.Fatal(err)
			}
			results := []domain.MetadataSearchResult{
				autoDiscoveryResult("igdb", "weak-game", "Neon Genesis Evangelion", domain.MediumGame),
				autoDiscoveryResult("tvmaze", "weak-series", "Evangelion Rebuild", domain.MediumVideo),
				autoDiscoveryResult("openlibrary", "weak-book", "End of Evangelion", domain.MediumLiterature),
			}
			for index := range results {
				results[index].Relevance = domain.SearchRelevanceWeak
			}
			var excludedWorkID domain.WorkID
			if test.excludeOneResult {
				work, err := repository.MaterializeExternalWork(ctx, resultMaterialization(results[2]))
				if err != nil {
					t.Fatal(err)
				}
				excludedWorkID = work.Work.ID
				if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
					UniverseID: target.ID, WorkID: work.Work.ID, Provenance: domain.UniverseMembershipProvenanceManual,
					Confidence: 1, ConfirmedByUser: true,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, target.ID, "excluded from this Universe"); err != nil {
					t.Fatal(err)
				}
			}

			discovery, err := application.AutoDiscover(ctx, "Evangelion", results)
			if err != nil {
				t.Fatal(err)
			}
			if discovery.State != test.wantState {
				t.Fatalf("Weak cohort discovery = %#v, want state %q", discovery, test.wantState)
			}
			if test.excludeOneResult && discovery.UniverseID != "" {
				t.Fatalf("excluded Weak evidence returned target id %q despite falling below threshold", discovery.UniverseID)
			}
			if !test.excludeOneResult && (discovery.UniverseID != target.ID || discovery.MembershipsAdded != 0) {
				t.Fatalf("unexcluded Weak evidence outcome = %#v, want target reuse without memberships", discovery)
			}
			graph, err := repository.GetUniverse(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(graph.Memberships) != 0 {
				t.Fatalf("Weak evidence produced %d target memberships", len(graph.Memberships))
			}
			if test.excludeOneResult {
				excluded, err := repository.IsUniverseWorkExcluded(ctx, excludedWorkID, target.ID)
				if err != nil || !excluded {
					t.Fatalf("excluded target evidence state = %t, %v; want true", excluded, err)
				}
			}
		})
	}
}

type autoDiscoverySearchProvider struct {
	name    string
	results []domain.MetadataSearchResult
}

func (provider autoDiscoverySearchProvider) Name() string { return provider.name }

func (provider autoDiscoverySearchProvider) Search(_ context.Context, _ string, limit int) ([]domain.MetadataSearchResult, error) {
	if len(provider.results) > limit {
		return provider.results[:limit], nil
	}
	return provider.results, nil
}

func TestAutoDiscoverPersistsSeparateHighUniverseAndWorkConfidence(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "auto-discovery.db")
	results := detectiveConanResults()
	weakScore := 1000000.0
	results[3].Relevance = domain.SearchRelevanceWeak
	results[3].Score = &weakScore
	results = append(results, domain.MetadataSearchResult{
		Provider: "openlibrary", ExternalID: "related-collision", Title: "Detective Conan Side Story",
		Medium: domain.MediumLiterature, MediaType: "book", WorkType: "book", Relevance: domain.SearchRelevanceRelated,
	})
	results[0].Relevance = domain.SearchRelevanceBest
	results[1].Relevance = domain.SearchRelevanceBest
	results[2].Relevance = domain.SearchRelevanceBest
	result, err := application.AutoDiscover(ctx, "Detective Conan", results)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.UniverseDiscoveryCreated || result.UniverseID == "" || result.MembershipsAdded != 3 {
		t.Fatalf("automatic discovery outcome = %#v, want created Universe and three strong memberships", result)
	}
	if result.ExistenceConfidence < 0.9 || result.ExistenceConfidence <= 0 || result.Title == "" {
		t.Fatalf("Universe existence confidence/title = %#v, want separate high-confidence values", result)
	}
	detail, err := application.GetUniverse(ctx, result.UniverseID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Universe.Provenance != domain.UniverseProvenanceAutomatic || detail.Universe.ConfirmedByUser ||
		detail.Universe.ExistenceConfidence != result.ExistenceConfidence {
		t.Fatalf("persisted Universe discovery metadata = %#v", detail.Universe)
	}
	if len(detail.Memberships) != 3 {
		t.Fatalf("persisted automatic memberships = %d, want 3: %#v", len(detail.Memberships), detail.Memberships)
	}
	wantedMemberships := map[string]bool{"game-1": true, "show-1": true, "book-1": true}
	var sawLowerMembershipConfidence bool
	for _, membership := range detail.Memberships {
		if !wantedMemberships[membership.ExternalID] {
			t.Errorf("non-Best Work %q was auto-materialized as a membership", membership.ExternalID)
		}
		delete(wantedMemberships, membership.ExternalID)
		if membership.Provenance != domain.UniverseMembershipProvenanceAutomatic || membership.ConfirmedByUser {
			t.Errorf("automatic membership provenance/confirmation = %q/%t", membership.Provenance, membership.ConfirmedByUser)
		}
		if membership.Evidence == "" || membership.Reason == "" {
			t.Errorf("persisted automatic membership evidence/reason = %q/%q, want non-empty decision record", membership.Evidence, membership.Reason)
		}
		if membership.Confidence < detail.Universe.ExistenceConfidence {
			sawLowerMembershipConfidence = true
		}
		if membership.ExternalID == "author-collision" {
			t.Error("description/creator collision was auto-materialized")
		}
	}
	if len(wantedMemberships) > 0 {
		t.Errorf("expected Best Works were not persisted: %#v", wantedMemberships)
	}
	if !sawLowerMembershipConfidence {
		t.Fatal("test data did not prove Universe existence confidence differs from Work membership confidence")
	}
	var works, identities, editions, assets int
	for query, target := range map[string]*int{
		"SELECT COUNT(*) FROM works":               &works,
		"SELECT COUNT(*) FROM external_identities": &identities,
		"SELECT COUNT(*) FROM editions":            &editions,
		"SELECT COUNT(*) FROM assets":              &assets,
	} {
		if err := db.QueryRowContext(ctx, query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if works != 3 || identities != 3 || editions != 0 || assets != 0 {
		t.Fatalf("automatic persistence fabricated catalog state: works=%d identities=%d editions=%d assets=%d", works, identities, editions, assets)
	}
}

func TestAutoDiscoverRefreshesOnlyLegacyUnknownAutomaticName(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "legacy-name-refresh.db")
	const universeID = "legacy-evangelion"
	if _, err := db.ExecContext(ctx, `INSERT INTO universes
		(id, title, existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user)
		VALUES (?, 'Detective Evangelion', 0.98, 0, 'none', 'legacy_unknown', 'automatic', 0)`, universeID); err != nil {
		t.Fatal(err)
	}
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "game", "Neon Genesis Evangelion", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "series", "Evangelion Rebuild", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "book", "End of Evangelion", domain.MediumLiterature),
	}
	for index := range results {
		results[index].Relevance = domain.SearchRelevanceWeak
	}

	discovery, err := application.AutoDiscover(ctx, "Evangelion", results)
	if err != nil {
		t.Fatalf("AutoDiscover() error = %v", err)
	}
	if discovery.State != domain.UniverseDiscoveryReused || discovery.UniverseID != universeID || discovery.Title != "Evangelion" {
		t.Fatalf("AutoDiscover() = %#v, want same ID with safe query title Evangelion", discovery)
	}
	detail, err := application.GetUniverse(ctx, universeID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Universe.Title != "Evangelion" || detail.Universe.NamingConfidence <= 0 ||
		detail.Universe.NamingConfidenceLevel == domain.UniverseNamingConfidenceNone ||
		detail.Universe.NamingEvidence == domain.UniverseNamingEvidenceLegacyUnknown {
		t.Fatalf("refreshed legacy name metadata = %#v, want safe persisted query naming evidence", detail.Universe)
	}
	var universeCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universeCount); err != nil {
		t.Fatal(err)
	}
	if universeCount != 1 {
		t.Fatalf("Universe count = %d, want same-ID refresh without duplicate creation", universeCount)
	}
}

func TestAutoDiscoverDoesNotRenameTrustedOrUserConfirmedUniverse(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name             string
		namingConfidence float64
		namingLevel      string
		namingEvidence   string
		provenance       string
		confirmedByUser  bool
	}{
		{name: "trusted automatic title", namingConfidence: 0.9, namingLevel: "high", namingEvidence: "distinctive_normalized_query", provenance: "automatic"},
		{name: "user-confirmed legacy title", namingConfidence: 0, namingLevel: "none", namingEvidence: "legacy_unknown", provenance: "automatic", confirmedByUser: true},
		{name: "manual title", namingConfidence: 1, namingLevel: "high", namingEvidence: "user_authored_title", provenance: "manual", confirmedByUser: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, application := newAutoDiscoveryApplication(t, test.name+".db")
			if _, err := application.repository.db.ExecContext(ctx, `INSERT INTO universes
					(id, title, existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user)
					VALUES (?, 'Detective Evangelion', 0.98, ?, ?, ?, ?, ?)`, test.name, test.namingConfidence, test.namingLevel, test.namingEvidence, test.provenance, test.confirmedByUser); err != nil {
				t.Fatal(err)
			}
			results := []domain.MetadataSearchResult{
				autoDiscoveryResult("igdb", "game", "Neon Genesis Evangelion", domain.MediumGame),
				autoDiscoveryResult("tvmaze", "series", "Evangelion Rebuild", domain.MediumVideo),
				autoDiscoveryResult("openlibrary", "book", "End of Evangelion", domain.MediumLiterature),
			}
			for index := range results {
				results[index].Relevance = domain.SearchRelevanceWeak
			}
			if _, err := application.AutoDiscover(ctx, "Evangelion", results); err != nil {
				t.Fatalf("AutoDiscover() error = %v", err)
			}
			detail, err := application.GetUniverse(ctx, domain.UniverseID(test.name))
			if err != nil {
				t.Fatal(err)
			}
			if detail.Universe.Title != "Detective Evangelion" || detail.Universe.NamingEvidence != domain.UniverseNamingEvidence(test.namingEvidence) ||
				detail.Universe.ConfirmedByUser != test.confirmedByUser {
				t.Fatalf("protected Universe metadata changed: %#v", detail.Universe)
			}
		})
	}
}

func TestAutoDiscoverIsIdempotentAndSurvivesDatabaseRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auto-discovery-restart.db")
	db, application := openAutoDiscoveryApplication(t, path)
	first, err := application.AutoDiscover(ctx, "Detective Conan", detectiveConanResults())
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "Detective Conan" || first.NamingConfidenceLevel != "high" || first.NamingEvidence == "" {
		t.Fatalf("first naming decision = %#v, want normalized query title with separate confidence evidence", first)
	}
	if _, err := application.RemoveWork(ctx, first.UniverseID, "igdb", "game-1", "acceptance exclusion fixture"); err != nil {
		t.Fatalf("record fixture exclusion before restart: %v", err)
	}
	second, err := application.AutoDiscover(ctx, "detective—conan", detectiveConanResults())
	if err != nil {
		t.Fatal(err)
	}
	if first.State != domain.UniverseDiscoveryCreated || second.State != domain.UniverseDiscoveryReused ||
		first.UniverseID != second.UniverseID || second.MembershipsAdded != 0 {
		t.Fatalf("repeat discovery = %#v then %#v; want same Universe and no duplicate memberships", first, second)
	}
	beforeRestart, err := application.GetUniverse(ctx, first.UniverseID)
	if err != nil {
		t.Fatalf("load discovery state before database reopen: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen discovery database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	application = NewUniverseApplication(NewSQLiteRepository(db))
	afterRestart, err := application.AutoDiscover(ctx, "Detective Conan", detectiveConanResults())
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.State != domain.UniverseDiscoveryReused || afterRestart.UniverseID != first.UniverseID || afterRestart.MembershipsAdded != 0 {
		t.Fatalf("discovery after restart = %#v, want same persistent Universe and no writes", afterRestart)
	}
	if afterRestart.Title != first.Title || afterRestart.ExistenceConfidence != first.ExistenceConfidence ||
		afterRestart.NamingConfidence != first.NamingConfidence || afterRestart.NamingEvidence != first.NamingEvidence ||
		afterRestart.Provenance != first.Provenance || afterRestart.ConfirmedByUser != first.ConfirmedByUser {
		t.Fatalf("discovery metadata after restart = %#v, want same title and independent confidence/provenance", afterRestart)
	}
	work, err := application.repository.GetWorkByExternalIdentity(ctx, "igdb", "game-1")
	if err != nil {
		t.Fatalf("load excluded Work after restart: %v", err)
	}
	if excluded, err := application.repository.IsUniverseWorkExcluded(ctx, work.Work.ID, first.UniverseID); err != nil || !excluded {
		t.Fatalf("exclusion after restart = %t, %v; want persistent exclusion", excluded, err)
	}
	detail, err := application.GetUniverse(ctx, first.UniverseID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Universe.ID != first.UniverseID || detail.Universe.Title != first.Title ||
		detail.Universe.ExistenceConfidence != first.ExistenceConfidence || detail.Universe.NamingConfidence != first.NamingConfidence ||
		detail.Universe.NamingEvidence != first.NamingEvidence || detail.Universe.Provenance != first.Provenance ||
		detail.Universe.ConfirmedByUser != first.ConfirmedByUser {
		t.Fatalf("persisted Universe after restart = %#v, want exact opaque ID/title/confidence/provenance", detail.Universe)
	}
	if len(detail.Memberships) != 2 || len(detail.Exclusions) != 1 {
		t.Fatalf("restart membership/exclusion state = %d/%d, want two memberships and one exclusion", len(detail.Memberships), len(detail.Exclusions))
	}
	if len(detail.Memberships) != len(beforeRestart.Memberships) || len(detail.Exclusions) != len(beforeRestart.Exclusions) {
		t.Fatalf("membership/exclusion counts changed across reopen: before=%d/%d after=%d/%d",
			len(beforeRestart.Memberships), len(beforeRestart.Exclusions), len(detail.Memberships), len(detail.Exclusions))
	}
	beforeMemberships := make(map[string]UniverseWorkRecord, len(beforeRestart.Memberships))
	for _, membership := range beforeRestart.Memberships {
		beforeMemberships[membership.Provider+"\x00"+membership.ExternalID] = membership
	}
	for _, membership := range detail.Memberships {
		before, found := beforeMemberships[membership.Provider+"\x00"+membership.ExternalID]
		if !found || membership.WorkID != before.WorkID || membership.Title != before.Title || membership.Medium != before.Medium ||
			membership.WorkType != before.WorkType || membership.Confidence != before.Confidence || membership.Evidence != before.Evidence ||
			membership.Reason != before.Reason || membership.Provenance != before.Provenance || membership.ConfirmedByUser != before.ConfirmedByUser {
			t.Errorf("membership after restart = %#v, want same persisted membership metadata as before %#v", membership, before)
		}
		if membership.Provider == "" || membership.ExternalID == "" || membership.Title == "" || membership.Medium == "" ||
			membership.Confidence <= 0 || membership.Evidence == "" || membership.Reason == "" ||
			membership.Provenance != domain.UniverseMembershipProvenanceAutomatic || membership.ConfirmedByUser {
			t.Errorf("membership metadata after restart = %#v, want full unconfirmed automatic decision record", membership)
		}
	}
	if detail.Exclusions[0].ExternalID != "game-1" || detail.Exclusions[0].Reason != "acceptance exclusion fixture" {
		t.Fatalf("exclusion after restart = %#v, want exact durable reason", detail.Exclusions[0])
	}
	if detail.Exclusions[0].Provider != beforeRestart.Exclusions[0].Provider ||
		detail.Exclusions[0].ExternalID != beforeRestart.Exclusions[0].ExternalID ||
		detail.Exclusions[0].Title != beforeRestart.Exclusions[0].Title ||
		detail.Exclusions[0].Medium != beforeRestart.Exclusions[0].Medium ||
		detail.Exclusions[0].WorkType != beforeRestart.Exclusions[0].WorkType ||
		detail.Exclusions[0].Reason != beforeRestart.Exclusions[0].Reason {
		t.Fatalf("exclusion after restart = %#v, want unchanged evidence from before reopen %#v", detail.Exclusions[0], beforeRestart.Exclusions[0])
	}
}

func TestAutoDiscoverUsesNormalizedQueryNameAndDeduplicatesCapitalizationPunctuation(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "game-of-thrones-normalized-name.db")
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "game-1", "Game of Thrones: A Telltale Games Series", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "show-1", "A Game of Thrones", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "book-1", "The Game of Thrones Companion", domain.MediumLiterature),
	}
	first, err := application.AutoDiscover(ctx, "game of thrones", results)
	if err != nil {
		t.Fatal(err)
	}
	second, err := application.AutoDiscover(ctx, "GAME—OF THRONES!", results)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != domain.UniverseDiscoveryCreated || second.State != domain.UniverseDiscoveryReused ||
		first.UniverseID == "" || first.UniverseID != second.UniverseID || first.Title != "Game of Thrones" || second.Title != first.Title ||
		first.MembershipsAdded != 3 || second.MembershipsAdded != 0 {
		t.Fatalf("normalized query discovery = %#v then %#v; want same opaque identity/title and idempotent membership state", first, second)
	}
}

func TestAutoDiscoverCreatesDistinctiveSingleTokenUniverseAcrossMedia(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "auto-evangelion.db")
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "game-1", "Neon Genesis Evangelion", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "show-1", "Evangelion Rebuild", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "book-1", "The End of Evangelion", domain.MediumLiterature),
		autoDiscoveryResult("openlibrary", "unrelated", "The Gospel of John", domain.MediumLiterature),
	}
	result, err := application.AutoDiscover(ctx, "Evangelion", results)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.UniverseDiscoveryCreated || result.MembershipsAdded != 3 || result.ExistenceConfidence != 0.98 {
		t.Fatalf("distinctive single-token discovery = %#v, want one high-confidence Universe with three exact whole-token memberships", result)
	}
	var universeCount, workCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universeCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM works").Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if universeCount != 1 || workCount != 3 {
		t.Fatalf("single-token discovery persisted universes=%d works=%d; want only three corroborated Works", universeCount, workCount)
	}
}

func TestConfirmUniversePreservesAutomaticEvidenceAndWorkDecisions(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "confirm-automatic-universe.db")
	results := detectiveConanResults()[:3]
	discovered, err := application.AutoDiscover(ctx, "Detective Conan", results)
	if err != nil || discovered.State != domain.UniverseDiscoveryCreated {
		t.Fatalf("AutoDiscover() = %#v, %v; want created Universe", discovered, err)
	}

	workConfirmation, err := application.ConfirmWork(ctx, discovered.UniverseID, resultMaterialization(results[0]))
	if err != nil {
		t.Fatal(err)
	}
	confirmedWork := findUniverseMembership(t, workConfirmation.Detail, results[0].ExternalID)
	if !confirmedWork.ConfirmedByUser || confirmedWork.Provenance != domain.UniverseMembershipProvenanceAutomatic ||
		confirmedWork.Confidence <= 0 || confirmedWork.Evidence == "" || confirmedWork.Reason == "" {
		t.Fatalf("explicit Work confirmation = %#v, want confirmed automatic evidence", confirmedWork)
	}
	if _, err := application.RejectWork(ctx, discovered.UniverseID, resultMaterialization(results[1]), "user excluded exact Work"); err != nil {
		t.Fatal(err)
	}

	before, err := application.GetUniverse(ctx, discovered.UniverseID)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := application.ConfirmUniverse(ctx, discovered.UniverseID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Universe.Provenance != domain.UniverseProvenanceAutomatic || !confirmed.Universe.ConfirmedByUser ||
		confirmed.Universe.ExistenceConfidence != before.Universe.ExistenceConfidence ||
		confirmed.Universe.NamingConfidence != before.Universe.NamingConfidence ||
		confirmed.Universe.NamingConfidenceLevel != before.Universe.NamingConfidenceLevel ||
		confirmed.Universe.NamingEvidence != before.Universe.NamingEvidence {
		t.Fatalf("explicit Universe confirmation = %#v, want only user-confirmed flag changed", confirmed.Universe)
	}
	if len(confirmed.Memberships) != 2 {
		t.Fatalf("memberships after Universe confirmation = %d, want two untouched accepted Works", len(confirmed.Memberships))
	}
	for _, membership := range confirmed.Memberships {
		beforeMembership := findUniverseMembership(t, before, membership.ExternalID)
		if membership.Provenance != beforeMembership.Provenance || membership.Confidence != beforeMembership.Confidence ||
			membership.Evidence != beforeMembership.Evidence || membership.Reason != beforeMembership.Reason ||
			membership.ConfirmedByUser != beforeMembership.ConfirmedByUser {
			t.Errorf("Universe confirmation changed membership %q: before=%#v after=%#v", membership.ExternalID, beforeMembership, membership)
		}
	}
	if len(confirmed.Exclusions) != 1 || confirmed.Exclusions[0].ExternalID != results[1].ExternalID {
		t.Fatalf("exclusions after Universe confirmation = %#v, want existing exclusion preserved", confirmed.Exclusions)
	}

	replayed, err := application.ConfirmUniverse(ctx, discovered.UniverseID)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Universe.UpdatedAt.Equal(confirmed.Universe.UpdatedAt) || !replayed.Universe.ConfirmedByUser {
		t.Fatalf("idempotent Universe confirmation changed update time/state: first=%#v replay=%#v", confirmed.Universe, replayed.Universe)
	}
}

func TestAutoDiscoverFailsClosedForNamedGenericAndAmbiguousQueries(t *testing.T) {
	for _, query := range []string{"game", "it", "up", "cars", "love", "conan", "detective"} {
		t.Run(query, func(t *testing.T) {
			ctx := context.Background()
			db, application := newAutoDiscoveryApplication(t, "generic-"+query+".db")
			results := []domain.MetadataSearchResult{
				autoDiscoveryResult("igdb", "game-1", query, domain.MediumGame),
				autoDiscoveryResult("tvmaze", "show-1", query, domain.MediumVideo),
				autoDiscoveryResult("openlibrary", "book-1", query, domain.MediumLiterature),
			}
			result, err := application.AutoDiscover(ctx, query, results)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != domain.UniverseDiscoveryNotEligible || result.UniverseID != "" {
				t.Fatalf("generic query discovery outcome = %#v, want not_eligible without persistence", result)
			}
			var universes, works int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universes); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM works").Scan(&works); err != nil {
				t.Fatal(err)
			}
			if universes != 0 || works != 0 {
				t.Fatalf("generic query persisted universes=%d works=%d, want 0/0", universes, works)
			}
		})
	}
}

func TestAutoDiscoverDoesNotReuseUnrelatedGenericLocalUniverse(t *testing.T) {
	for _, test := range []struct {
		name         string
		title        string
		seedWrongKey bool
	}{
		{name: "matching title token", title: "Game of Thrones"},
		{name: "noncanonical discovery key", title: "Existing Collection", seedWrongKey: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, application := newAutoDiscoveryApplication(t, "generic-local-"+strings.ReplaceAll(test.name, " ", "-")+".db")
			existing := domain.Universe{ID: "existing-universe", Title: test.title}
			if err := application.repository.CreateUniverse(ctx, existing); err != nil {
				t.Fatal(err)
			}
			if test.seedWrongKey {
				if _, err := db.ExecContext(ctx, `INSERT INTO universe_discovery_keys (normalized_key, universe_id, created_at_utc)
					VALUES (?, ?, ?)`, "game", existing.ID, formatUniverseTime(time.Now().UTC())); err != nil {
					t.Fatal(err)
				}
			}

			got, err := application.AutoDiscover(ctx, "game", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.UniverseDiscoveryNotEligible || got.UniverseID != "" || got.MembershipsAdded != 0 {
				t.Fatalf("generic local lookup = %#v, want no unrelated Universe reuse", got)
			}
			var universes, works, memberships int
			for query, target := range map[string]*int{
				"SELECT COUNT(*) FROM universes":            &universes,
				"SELECT COUNT(*) FROM works":                &works,
				"SELECT COUNT(*) FROM universe_memberships": &memberships,
			} {
				if err := db.QueryRowContext(ctx, query).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			if universes != 1 || works != 0 || memberships != 0 {
				t.Fatalf("generic local lookup changed catalog: universes=%d works=%d memberships=%d", universes, works, memberships)
			}
		})
	}
}

func TestAutoDiscoverReusesDistinctiveDiscoveryKeyAfterUniverseRename(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
		key   string
		title string
	}{
		{name: "distinctive multi-token query", query: "Game of Thrones", key: "game of thrones", title: "Renamed Display Title"},
		{name: "distinctive single-token query", query: "Evangelion", key: "evangelion", title: "Rebranded Series"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, application := newAutoDiscoveryApplication(t, "distinctive-key-"+strings.ReplaceAll(test.name, " ", "-")+".db")
			want := domain.Universe{ID: "stable-universe-id", Title: test.title}
			if err := application.repository.CreateUniverse(ctx, want); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO universe_discovery_keys (normalized_key, universe_id, created_at_utc)
				VALUES (?, ?, ?)`, test.key, want.ID, formatUniverseTime(time.Now().UTC())); err != nil {
				t.Fatal(err)
			}

			got, err := application.AutoDiscover(ctx, test.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.UniverseDiscoveryReused || got.UniverseID != want.ID || got.MembershipsAdded != 0 {
				t.Fatalf("renamed Universe discovery = %#v, want reuse of %q without membership changes", got, want.ID)
			}
			var universeCount, membershipCount int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universeCount); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universe_memberships").Scan(&membershipCount); err != nil {
				t.Fatal(err)
			}
			if universeCount != 1 || membershipCount != 0 {
				t.Fatalf("renamed Universe lookup changed catalog: universes=%d memberships=%d", universeCount, membershipCount)
			}
		})
	}
}

func TestAutoDiscoverReusesCanonicalNormalizedLocalUniverseTitle(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "canonical-normalized-local-universe.db")
	want := domain.Universe{ID: "game-of-thrones", Title: "Game of Thrones"}
	if err := application.repository.CreateUniverse(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := application.AutoDiscover(ctx, "GAME—OF THRONES", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.UniverseDiscoveryReused || got.UniverseID != want.ID || got.MembershipsAdded != 0 {
		t.Fatalf("canonical normalized local title = %#v, want reuse of %q without membership changes", got, want.ID)
	}
}

func TestAutoDiscoverDoesNotMaterializeCollectionWithoutSafeName(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "no-safe-universe-name.db")
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "game-1", "The Game", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "show-1", "The Game", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "book-1", "The Game", domain.MediumLiterature),
	}
	result, err := application.AutoDiscover(ctx, "the game", results)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.UniverseDiscoveryNotEligible || result.UniverseID != "" || result.Title != "" {
		t.Fatalf("unsafe-name discovery result = %#v, want no auto Universe and no inferred title", result)
	}
	var universes, works int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if universes != 0 || works != 0 {
		t.Fatalf("unsafe name persisted universes=%d works=%d, want neither materialized", universes, works)
	}
}

func TestAutoDiscoverSurfacesExistingGenericUniverseWithoutCreatingMemberships(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "generic-existing-universe.db")
	want := domain.Universe{ID: "detective-conan", Title: "Detective Conan"}
	if err := application.repository.CreateUniverse(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := application.AutoDiscover(ctx, "Conan", detectiveConanResults())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.UniverseDiscoveryReused || got.UniverseID != want.ID || got.MembershipsAdded != 0 ||
		got.Provenance != domain.UniverseProvenanceManual || !got.ConfirmedByUser {
		t.Fatalf("existing generic Universe outcome = %#v, want existing %q and no automatic membership changes", got, want.ID)
	}
	var universes, works, memberships int
	for query, target := range map[string]*int{
		"SELECT COUNT(*) FROM universes":            &universes,
		"SELECT COUNT(*) FROM works":                &works,
		"SELECT COUNT(*) FROM universe_memberships": &memberships,
	} {
		if err := db.QueryRowContext(ctx, query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if universes != 1 || works != 0 || memberships != 0 {
		t.Fatalf("generic existing lookup mutated catalog: universes=%d works=%d memberships=%d", universes, works, memberships)
	}
}

func TestAutoDiscoverFailsClosedForAmbiguousGenericLocalUniverseMatches(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "generic-ambiguous-existing-universes.db")
	for _, record := range []domain.Universe{
		{ID: "detective-conan", Title: "Detective Conan"},
		{ID: "conan-collection", Title: "Conan Collection"},
	} {
		if err := application.repository.CreateUniverse(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	got, err := application.AutoDiscover(ctx, "Conan", detectiveConanResults())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.UniverseDiscoveryAmbiguous || got.UniverseID != "" || got.MembershipsAdded != 0 {
		t.Fatalf("ambiguous generic local match = %#v, want fail-closed ambiguity without identity or mutations", got)
	}
}

func TestAutoDiscoverReusesOnlyOneUnambiguousAliasMatch(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "auto-alias-reuse.db")
	repository := application.repository
	first := domain.Universe{ID: "detective-conan", Title: "Detective Conan"}
	second := domain.Universe{ID: "case-closed", Title: "Other Adventure Universe"}
	for _, universe := range []domain.Universe{first, second} {
		if err := repository.CreateUniverse(ctx, universe); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{UniverseID: first.ID, Alias: "Case Closed", Provenance: "manual"}); err != nil {
		t.Fatal(err)
	}
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "game-1", "Case Closed", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "show-1", "Case Closed", domain.MediumVideo),
	}
	result, err := application.AutoDiscover(ctx, "case—closed", results)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.UniverseDiscoveryReused || result.UniverseID != first.ID || result.Title != first.Title {
		t.Fatalf("unique explicit alias outcome = %#v, want reuse of %q", result, first.ID)
	}
	var membershipsBeforeConflict int
	if err := application.repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universe_memberships").Scan(&membershipsBeforeConflict); err != nil {
		t.Fatal(err)
	}

	if _, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{UniverseID: second.ID, Alias: "Case Closed", Provenance: "manual"}); err != nil {
		t.Fatal(err)
	}
	ambiguous, err := application.AutoDiscover(ctx, "case—closed", results)
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.State != domain.UniverseDiscoveryAmbiguous || ambiguous.UniverseID != "" {
		t.Fatalf("conflicting aliases outcome = %#v, want fail-closed ambiguous", ambiguous)
	}
	var universeCount, membershipCount int
	if err := application.repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universes").Scan(&universeCount); err != nil {
		t.Fatal(err)
	}
	if err := application.repository.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM universe_memberships").Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if universeCount != 2 || membershipCount != membershipsBeforeConflict {
		t.Fatalf("ambiguous alias mutation left universes=%d memberships=%d, want 2/%d", universeCount, membershipCount, membershipsBeforeConflict)
	}
}

func TestAutoDiscoverPreservesManualMembershipsExclusionsAndOtherUniverses(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "auto-preserve-decisions.db")
	repository := application.repository
	target := domain.Universe{ID: "evangelion", Title: "Neon Genesis Evangelion"}
	other := domain.Universe{ID: "other", Title: "Other Universe"}
	for _, universe := range []domain.Universe{target, other} {
		if err := repository.CreateUniverse(ctx, universe); err != nil {
			t.Fatal(err)
		}
	}
	results := []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "manual", "Neon Genesis Evangelion", domain.MediumGame),
		autoDiscoveryResult("openlibrary", "elsewhere", "The End of Evangelion", domain.MediumLiterature),
		autoDiscoveryResult("tvmaze", "excluded", "Evangelion Rebuild", domain.MediumVideo),
		autoDiscoveryResult("provider", "existing-unconfirmed", "Evangelion Radio Drama", domain.MediumAudio),
		autoDiscoveryResult("tvmaze", "new-tv", "Evangelion: New TV Work", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "new-book", "A Work About Evangelion", domain.MediumLiterature),
	}
	var workIDs = make(map[string]domain.WorkID)
	for _, result := range results[:4] {
		graph, err := repository.MaterializeExternalWork(ctx, resultMaterialization(result))
		if err != nil {
			t.Fatal(err)
		}
		workIDs[result.ExternalID] = graph.Work.ID
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: target.ID, WorkID: workIDs["manual"], Provenance: domain.UniverseMembershipProvenanceManual,
		Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: other.ID, WorkID: workIDs["elsewhere"], Provenance: domain.UniverseMembershipProvenanceManual,
		Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: target.ID, WorkID: workIDs["existing-unconfirmed"], Provenance: domain.UniverseMembershipProvenanceProvider,
		Confidence: 0.7, Evidence: "provider_import", Reason: "Provider supplied the exact Work relationship.", ConfirmedByUser: false,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RemoveUniverseMembership(ctx, workIDs["excluded"], target.ID, "user rejected this Work"); err != nil {
		t.Fatal(err)
	}

	result, err := application.AutoDiscover(ctx, "Evangelion", results)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.UniverseDiscoveryReused || result.UniverseID != target.ID || result.MembershipsAdded != 2 {
		t.Fatalf("protected decision discovery = %#v, want reuse and only two automatic additions", result)
	}
	manual, err := repository.GetWorkUniverseMembership(ctx, workIDs["manual"])
	if err != nil || manual.UniverseID != target.ID || manual.Provenance != domain.UniverseMembershipProvenanceManual || !manual.ConfirmedByUser ||
		manual.Evidence != "user_confirmed_membership" || manual.Reason == "" {
		t.Fatalf("manual membership changed: %#v, %v", manual, err)
	}
	elsewhere, err := repository.GetWorkUniverseMembership(ctx, workIDs["elsewhere"])
	if err != nil || elsewhere.UniverseID != other.ID || elsewhere.Provenance != domain.UniverseMembershipProvenanceManual || !elsewhere.ConfirmedByUser ||
		elsewhere.Evidence != "user_confirmed_membership" || elsewhere.Reason == "" {
		t.Fatalf("other-Universe membership changed: %#v, %v", elsewhere, err)
	}
	excluded, err := repository.IsUniverseWorkExcluded(ctx, workIDs["excluded"], target.ID)
	if err != nil || !excluded {
		t.Fatalf("explicit exclusion = %t, %v; want preserved true", excluded, err)
	}
	existingUnconfirmed, err := repository.GetWorkUniverseMembership(ctx, workIDs["existing-unconfirmed"])
	if err != nil || existingUnconfirmed.UniverseID != target.ID || existingUnconfirmed.Provenance != domain.UniverseMembershipProvenanceProvider ||
		existingUnconfirmed.Confidence != 0.7 || existingUnconfirmed.Evidence != "provider_import" ||
		existingUnconfirmed.Reason != "Provider supplied the exact Work relationship." || existingUnconfirmed.ConfirmedByUser {
		t.Fatalf("existing non-user-confirmed membership changed: %#v, %v", existingUnconfirmed, err)
	}
	if _, err := repository.GetWorkUniverseMembership(ctx, workIDs["excluded"]); !errors.Is(err, ErrUniverseMembershipNotFound) {
		t.Fatalf("excluded Work membership error = %v, want no membership", err)
	}
	graph, err := repository.GetUniverse(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, membership := range graph.Memberships {
		if membership.Provenance == domain.UniverseMembershipProvenanceAutomatic && membership.ConfirmedByUser {
			t.Errorf("automatic membership was user-confirmed: %#v", membership)
		}
	}
}

func TestAutoDiscoverRollsBackUniverseWorksAndMembershipsOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, application := newAutoDiscoveryApplication(t, "auto-atomic.db")
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_automatic_membership BEFORE INSERT ON universe_memberships
		WHEN NEW.provenance = 'automatic' BEGIN SELECT RAISE(ABORT, 'fixture rejects automatic membership'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := application.AutoDiscover(ctx, "Detective Conan", detectiveConanResults()); err == nil {
		t.Fatal("discovery succeeded despite injected membership failure")
	}
	for _, table := range []string{"universes", "works", "external_identities", "universe_memberships", "universe_discovery_keys"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("failed discovery left %d rows in %s", count, table)
		}
	}
}

func TestUniverseResolveAcceptsWebLimitAndRejectsOneOver(t *testing.T) {
	ctx := context.Background()
	_, application := newAutoDiscoveryApplication(t, "resolver-result-boundary.db")
	results := make([]domain.MetadataSearchResult, 20)
	for index := range results {
		results[index] = autoDiscoveryResult("provider", fmt.Sprint(index), "Neon Genesis Evangelion", domain.MediumVideo)
	}
	if _, err := application.Resolve(ctx, "Neon Genesis Evangelion", results); err != nil {
		t.Fatalf("resolve at 20-result Web boundary: %v", err)
	}
	if _, err := application.AutoDiscover(ctx, "Detective Conan", results); err != nil {
		t.Fatalf("automatic discovery at 20-result Web boundary: %v", err)
	}
	results = append(results, autoDiscoveryResult("provider", "extra", "Neon Genesis Evangelion", domain.MediumVideo))
	if _, err := application.Resolve(ctx, "Neon Genesis Evangelion", results); err == nil {
		t.Fatal("resolve accepted 21 results when the Web parser cap is 20")
	}
	tooMany := make([]domain.MetadataSearchResult, 21)
	for index := range tooMany {
		tooMany[index] = autoDiscoveryResult("provider", fmt.Sprint(index), "Detective Conan", domain.MediumVideo)
	}
	if _, err := application.AutoDiscover(ctx, "Detective Conan", tooMany); err == nil {
		t.Fatal("automatic discovery accepted 21 results when the Web parser cap is 20")
	}
}

func TestUniverseResolveBoundsOutputCandidatesAtWebLimit(t *testing.T) {
	ctx := context.Background()
	result := []domain.MetadataSearchResult{
		autoDiscoveryResult("provider", "work-1", "Neon Genesis Evangelion", domain.MediumVideo),
	}
	for _, universeCount := range []int{20, 21} {
		t.Run(fmt.Sprintf("%d candidates", universeCount), func(t *testing.T) {
			_, application := newAutoDiscoveryApplication(t, fmt.Sprintf("resolver-output-%d.db", universeCount))
			for index := 0; index < universeCount; index++ {
				universe := domain.Universe{
					ID: domain.UniverseID(fmt.Sprintf("opaque-%02d", index)), Title: "Neon Genesis Evangelion",
				}
				if err := application.repository.CreateUniverse(ctx, universe); err != nil {
					t.Fatal(err)
				}
			}
			candidates, err := application.Resolve(ctx, "Neon Genesis Evangelion", result)
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 20 {
				t.Fatalf("catalog resolver returned %d candidates from %d local Universes, want Web limit 20", len(candidates), universeCount)
			}
			for index, candidate := range candidates {
				wantID := domain.UniverseID(fmt.Sprintf("opaque-%02d", index))
				if candidate.UniverseID != wantID {
					t.Errorf("catalog candidate[%d].UniverseID = %q, want deterministic bounded candidate %q", index, candidate.UniverseID, wantID)
				}
			}
		})
	}
}

func newAutoDiscoveryApplication(t *testing.T, name string) (*sql.DB, *UniverseApplication) {
	t.Helper()
	return openAutoDiscoveryApplication(t, filepath.Join(t.TempDir(), name))
}

func openAutoDiscoveryApplication(t *testing.T, path string) (*sql.DB, *UniverseApplication) {
	t.Helper()
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewUniverseApplication(NewSQLiteRepository(db))
}

func findUniverseMembership(t *testing.T, detail UniverseDetail, externalID string) UniverseWorkRecord {
	t.Helper()
	for _, membership := range detail.Memberships {
		if membership.ExternalID == externalID {
			return membership
		}
	}
	t.Fatalf("Universe %q has no membership for exact external ID %q", detail.Universe.ID, externalID)
	return UniverseWorkRecord{}
}

func detectiveConanResults() []domain.MetadataSearchResult {
	return []domain.MetadataSearchResult{
		autoDiscoveryResult("igdb", "game-1", "Case Closed: Detective Conan", domain.MediumGame),
		autoDiscoveryResult("tvmaze", "show-1", "Detective Conan: The Movie", domain.MediumVideo),
		autoDiscoveryResult("openlibrary", "book-1", "Detective Conan and the Vanished", domain.MediumLiterature),
		autoDiscoveryResult("openlibrary", "author-collision", "Arthur Conan Doyle's Sherlock Holmes", domain.MediumLiterature),
	}
}

func autoDiscoveryResult(provider, externalID, title string, medium domain.Medium) domain.MetadataSearchResult {
	workType := map[domain.Medium]string{
		domain.MediumGame: "game", domain.MediumVideo: "series", domain.MediumLiterature: "book", domain.MediumAudio: "music",
	}[medium]
	return domain.MetadataSearchResult{
		Provider: provider, ExternalID: externalID, Title: title, Medium: medium, MediaType: workType, WorkType: workType,
		Relevance: domain.SearchRelevanceBest,
	}
}

func resultMaterialization(result domain.MetadataSearchResult) domain.WorkMaterialization {
	return domain.WorkMaterialization{
		Provider: result.Provider, ExternalID: result.ExternalID, Title: result.Title,
		Medium: result.Medium, WorkType: result.WorkType,
	}
}
