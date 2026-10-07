package universe

import (
	"fmt"
	"testing"

	"lernae/internal/domain"
)

func TestAssessAutomaticDiscoveryNeedsIndependentStrongAnchors(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "Case Closed: Detective Conan", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "Detective Conan: The Movie", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "Detective Conan and the Vanished", domain.MediumLiterature),
		discoveryHit("openlibrary", "author-collision", "Arthur Conan Doyle's Sherlock Holmes", domain.MediumLiterature),
	}
	results[3].Creators = []string{"Detective Conan"}
	results[3].Summary = "Detective Conan appears in this unrelated description."

	candidate, ok := AssessAutomaticDiscovery("Detective Conan", results)
	if !ok {
		t.Fatal("AssessAutomaticDiscovery() rejected two independent exact phrase anchors")
	}
	if candidate.Title == "" || candidate.ExistenceConfidence < 0.9 || candidate.ConfidenceLevel != ConfidenceHigh {
		t.Fatalf("discovery confidence/title = %#v, want a high-confidence Universe candidate", candidate)
	}
	if len(candidate.Memberships) != 3 {
		t.Fatalf("auto memberships = %d, want only three title-anchored Works: %#v", len(candidate.Memberships), candidate.Memberships)
	}
	for _, membership := range candidate.Memberships {
		if membership.ConfidenceLevel != ConfidenceHigh || membership.Confidence < 0.85 {
			t.Errorf("automatic membership confidence = %v/%q, want high and independent of Universe confidence", membership.Confidence, membership.ConfidenceLevel)
		}
		if membership.Work.ExternalID == "author-collision" {
			t.Error("creator/summary-only collision was included as an automatic membership")
		}
	}
}

func TestAssessAutomaticDiscoveryAllowsDistinctiveSingleTokenOnlyWithCrossMediaCohort(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "Neon Genesis Evangelion", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "Evangelion Rebuild", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "The End of Evangelion", domain.MediumLiterature),
		discoveryHit("openlibrary", "unrelated", "The Gospel of John", domain.MediumLiterature),
	}
	candidate, ok := AssessAutomaticDiscovery("Evangelion", results)
	if !ok {
		t.Fatal("AssessAutomaticDiscovery() rejected three whole-token anchors across three media")
	}
	if len(candidate.Memberships) != 3 {
		t.Fatalf("automatic membership count = %d, want three whole-token anchors", len(candidate.Memberships))
	}
	if candidate.Title != "Evangelion" {
		t.Fatalf("automatic Universe title = %q, want the distinctive query rather than a Work title", candidate.Title)
	}
	if candidate.NamingConfidenceLevel != ConfidenceHigh || candidate.NamingConfidence == candidate.ExistenceConfidence {
		t.Fatalf("naming confidence = %v/%q, existence confidence = %v; want independent typed confidence", candidate.NamingConfidence, candidate.NamingConfidenceLevel, candidate.ExistenceConfidence)
	}
	for _, membership := range candidate.Memberships {
		if membership.Evidence != EvidenceDistinctiveToken {
			t.Errorf("single-token membership evidence = %q, want exact whole-token evidence", membership.Evidence)
		}
	}
}

func TestAssessAutomaticDiscoveryUsesDistinctiveQueryNamesWithoutFranchiseAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		results []domain.MetadataSearchResult
		want    string
	}{
		{
			name:  "Evangelion is not named after Detective Evangelion",
			query: "Evangelion",
			results: []domain.MetadataSearchResult{
				discoveryHit("igdb", "game-1", "Detective Evangelion: Case Files", domain.MediumGame),
				discoveryHit("tvmaze", "show-1", "Neon Genesis Evangelion", domain.MediumVideo),
				discoveryHit("openlibrary", "book-1", "The End of Evangelion", domain.MediumLiterature),
			},
			want: "Evangelion",
		},
		{
			name:  "Game of Thrones is named from the query without a whitelist",
			query: "game of thrones",
			results: []domain.MetadataSearchResult{
				discoveryHit("igdb", "game-1", "Game of Thrones: A Telltale Games Series", domain.MediumGame),
				discoveryHit("tvmaze", "show-1", "A Game of Thrones", domain.MediumVideo),
				discoveryHit("openlibrary", "book-1", "The Game of Thrones Companion", domain.MediumLiterature),
			},
			want: "Game of Thrones",
		},
		{
			name:  "Detective Conan is named from the query",
			query: "Detective Conan",
			results: []domain.MetadataSearchResult{
				discoveryHit("igdb", "game-1", "Case Closed: Detective Conan", domain.MediumGame),
				discoveryHit("tvmaze", "show-1", "Detective Conan: The Movie", domain.MediumVideo),
				discoveryHit("openlibrary", "book-1", "Detective Conan and the Vanished", domain.MediumLiterature),
			},
			want: "Detective Conan",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, ok := AssessAutomaticDiscovery(test.query, test.results)
			if !ok {
				t.Fatal("AssessAutomaticDiscovery() rejected a distinctive query with high-confidence membership anchors")
			}
			if candidate.Title != test.want {
				t.Fatalf("Universe title = %q, want safe query name %q", candidate.Title, test.want)
			}
			if candidate.NamingConfidenceLevel != ConfidenceHigh || candidate.NamingEvidence == "" {
				t.Fatalf("naming decision = confidence %v/%q evidence %q, want independent high-confidence query evidence", candidate.NamingConfidence, candidate.NamingConfidenceLevel, candidate.NamingEvidence)
			}
		})
	}
}

func TestAssessAutomaticDiscoveryExcludesIncidentalWholePhraseTitle(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "Game of Thrones: A Telltale Games Series", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "A Game of Thrones", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "The Game of Thrones Companion", domain.MediumLiterature),
		discoveryHit("tvmaze", "22602", "Hip Hop Tribe 2 : Game of Thrones", domain.MediumVideo),
	}
	// Model the pre-fix Search classifier output as well: discovery must not
	// independently turn an incidental embedded phrase into membership proof.
	results[3].Relevance = domain.SearchRelevanceBest

	candidate, ok := AssessAutomaticDiscovery("Game of Thrones", results)
	if !ok {
		t.Fatal("AssessAutomaticDiscovery() rejected the legitimate franchise-leading candidates")
	}
	if candidate.Title != "Game of Thrones" {
		t.Fatalf("Universe title = %q, want query-derived title Game of Thrones", candidate.Title)
	}
	wantMembers := map[string]bool{
		"igdb:game-1": true, "tvmaze:show-1": true, "openlibrary:book-1": true,
	}
	for _, membership := range candidate.Memberships {
		identity := membership.Work.Provider + ":" + membership.Work.ExternalID
		if identity == "tvmaze:22602" {
			t.Fatal("incidental Game of Thrones phrase became an automatic membership")
		}
		if !wantMembers[identity] {
			t.Errorf("unexpected automatic membership %q", identity)
		}
		delete(wantMembers, identity)
	}
	if len(wantMembers) != 0 {
		t.Fatalf("legitimate franchise-leading memberships missing: %v", wantMembers)
	}
}

func TestAssessAutomaticDiscoveryUsesSharedHighConfidenceAnchorWhenQueryCannotNameCollection(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "The Game of Thrones", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "The Game of Thrones Companion", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "The Game of Thrones Journal", domain.MediumLiterature),
	}
	candidate, ok := AssessAutomaticDiscovery("the game", results)
	if !ok {
		t.Fatal("AssessAutomaticDiscovery() rejected the shared distinctive title anchor")
	}
	if candidate.Title != "The Game of Thrones" || candidate.NamingEvidence != domain.UniverseNamingEvidenceSharedAnchor ||
		candidate.NamingConfidenceLevel != ConfidenceHigh || candidate.NamingConfidence == candidate.ExistenceConfidence {
		t.Fatalf("shared-anchor naming decision = %#v, want an independently confident shared distinctive anchor", candidate)
	}
}

func TestAssessAutomaticDiscoveryFailsClosedWhenNoSafeNameExists(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "The Game", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "The Game", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "The Game", domain.MediumLiterature),
	}
	if candidate, ok := AssessAutomaticDiscovery("the game", results); ok {
		t.Fatalf("query without a distinctive query name or shared anchor created %#v", candidate)
	}
}

func TestAssessAutomaticDiscoveryKeepsRelatedAndWeakResultsAsSuggestions(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "Case Closed: Detective Conan", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "Detective Conan: The Movie", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "Detective Conan and the Vanished", domain.MediumLiterature),
		discoveryHit("provider-related", "related", "Detective Conan Side Story", domain.MediumAudio),
		discoveryHit("provider-weak", "weak", "Detective Conan Special Edition", domain.MediumGame),
	}
	for index := range results[:3] {
		results[index].Relevance = domain.SearchRelevanceBest
	}
	results[3].Relevance = domain.SearchRelevanceRelated
	results[4].Relevance = domain.SearchRelevanceWeak

	candidate, ok := AssessAutomaticDiscovery("Detective Conan", results)
	if !ok {
		t.Fatal("AssessAutomaticDiscovery() rejected the three Best exact-identity anchors")
	}
	if len(candidate.Memberships) != 3 {
		t.Fatalf("automatic memberships = %d, want only three Best results: %#v", len(candidate.Memberships), candidate.Memberships)
	}
	for _, membership := range candidate.Memberships {
		if membership.Work.ExternalID == "related" || membership.Work.ExternalID == "weak" {
			t.Errorf("non-Best result %q became an automatic membership", membership.Work.ExternalID)
		}
	}
}

func TestAssessAutomaticDiscoveryUsesOnlyExplicitProviderAliases(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "Detective Conan: The Movie", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "Detective Conan Special", domain.MediumVideo),
	}
	results[0].Aliases = []string{"Case Closed"}
	results[1].Aliases = []string{"Case Closed"}
	results[0].Summary = "Also known as Case Closed"
	results[1].Creators = []string{"Case Closed"}

	candidate, ok := AssessAutomaticDiscovery("Case Closed", results)
	if !ok {
		t.Fatal("explicit provider aliases across independent Works did not qualify")
	}
	if len(candidate.Memberships) != 2 {
		t.Fatalf("explicit alias memberships = %d, want 2", len(candidate.Memberships))
	}
	for _, membership := range candidate.Memberships {
		if membership.Evidence != EvidenceExplicitAlias {
			t.Errorf("alias membership evidence = %q, want explicit alias", membership.Evidence)
		}
	}
}

func TestAssessAutomaticDiscoveryRejectsGenericAndAmbiguousQueries(t *testing.T) {
	for _, query := range []string{"game", "it", "up", "cars", "love", "conan", "detective"} {
		t.Run(query, func(t *testing.T) {
			results := []domain.MetadataSearchResult{
				discoveryHit("igdb", "game-1", query, domain.MediumGame),
				discoveryHit("tvmaze", "show-1", query, domain.MediumVideo),
				discoveryHit("openlibrary", "book-1", query, domain.MediumLiterature),
			}
			if candidate, ok := AssessAutomaticDiscovery(query, results); ok {
				t.Fatalf("generic/ambiguous query created candidate %#v", candidate)
			}
		})
	}
}

func TestAssessAutomaticDiscoveryRejectsDominantUnrelatedResultsAndSingleMedia(t *testing.T) {
	anchored := []domain.MetadataSearchResult{
		discoveryHit("igdb", "game-1", "Neon Genesis Evangelion", domain.MediumGame),
		discoveryHit("tvmaze", "show-1", "Evangelion Rebuild", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "The End of Evangelion", domain.MediumLiterature),
	}
	dominantUnrelated := append(append([]domain.MetadataSearchResult(nil), anchored...),
		discoveryHit("provider-a", "unrelated-1", "Quiet River", domain.MediumGame),
		discoveryHit("provider-b", "unrelated-2", "Silver Harbor", domain.MediumVideo),
		discoveryHit("provider-c", "unrelated-3", "Distant Mountain", domain.MediumLiterature),
	)
	if candidate, ok := AssessAutomaticDiscovery("Evangelion", dominantUnrelated); ok {
		t.Fatalf("dominant unrelated results created a candidate %#v", candidate)
	}
	sameMedium := []domain.MetadataSearchResult{
		discoveryHit("igdb", "one", "Neon Genesis Evangelion", domain.MediumGame),
		discoveryHit("tvmaze", "two", "Evangelion Rebuild", domain.MediumGame),
		discoveryHit("openlibrary", "three", "The End of Evangelion", domain.MediumGame),
	}
	if candidate, ok := AssessAutomaticDiscovery("Evangelion", sameMedium); ok {
		t.Fatalf("single-medium results created a candidate %#v", candidate)
	}
}

func TestAssessAutomaticDiscoveryDeduplicatesExactProviderIdentityAndBoundsResults(t *testing.T) {
	duplicate := discoveryHit("igdb", "same-work", "Neon Genesis Evangelion", domain.MediumGame)
	results := []domain.MetadataSearchResult{
		duplicate,
		duplicate,
		discoveryHit("tvmaze", "show-1", "Evangelion Rebuild", domain.MediumVideo),
	}
	if candidate, ok := AssessAutomaticDiscovery("Evangelion", results); ok {
		t.Fatalf("duplicate provider identity inflated a single-token cohort: %#v", candidate)
	}
	ambiguousIdentity := []domain.MetadataSearchResult{
		duplicate,
		discoveryHit("igdb", "same-work", "Neon Genesis Evangelion", domain.MediumVideo),
		discoveryHit("tvmaze", "show-1", "Evangelion Rebuild", domain.MediumVideo),
		discoveryHit("openlibrary", "book-1", "The End of Evangelion", domain.MediumLiterature),
	}
	if candidate, ok := AssessAutomaticDiscovery("Evangelion", ambiguousIdentity); ok {
		t.Fatalf("conflicting medium data for one exact provider identity created a candidate %#v", candidate)
	}
	tooMany := make([]domain.MetadataSearchResult, MaxCandidateResults+1)
	for index := range tooMany {
		tooMany[index] = discoveryHit("provider", fmt.Sprint(index), "Detective Conan", domain.MediumVideo)
	}
	if candidate, ok := AssessAutomaticDiscovery("Detective Conan", tooMany); ok {
		t.Fatalf("out-of-bounds input created a candidate %#v", candidate)
	}
}

func TestResolveFailsClosedAboveWebCandidateResultLimit(t *testing.T) {
	results := make([]domain.MetadataSearchResult, MaxCandidateResults)
	for index := range results {
		results[index] = discoveryHit("provider", fmt.Sprint(index), "Neon Genesis Evangelion", domain.MediumVideo)
	}
	if candidates := Resolve(ResolveInput{Query: "Neon Genesis Evangelion", Results: results}); len(candidates) == 0 {
		t.Fatal("Resolve() rejected the inclusive 20-result boundary")
	}
	results = append(results, discoveryHit("provider", "extra", "Neon Genesis Evangelion", domain.MediumVideo))
	if candidates := Resolve(ResolveInput{Query: "Neon Genesis Evangelion", Results: results}); len(candidates) != 0 {
		t.Fatalf("Resolve() accepted %d results above the Web parser cap of %d", len(results), MaxCandidateResults)
	}
}

func TestResolveBoundsOutputCandidateCountAtWebLimit(t *testing.T) {
	results := []domain.MetadataSearchResult{
		discoveryHit("provider", "one-result", "Neon Genesis Evangelion", domain.MediumVideo),
	}
	for _, count := range []int{MaxUniverseCandidates, MaxUniverseCandidates + 1} {
		t.Run(fmt.Sprintf("%d candidates", count), func(t *testing.T) {
			universes := make([]UniverseState, count)
			for index := range universes {
				universes[index] = UniverseState{Universe: domain.Universe{
					ID: domain.UniverseID(fmt.Sprintf("universe-%02d", index)), Title: "Neon Genesis Evangelion",
				}}
			}
			candidates := Resolve(ResolveInput{Query: "Neon Genesis Evangelion", Results: results, Universes: universes})
			want := count
			if want > MaxUniverseCandidates {
				want = MaxUniverseCandidates
			}
			if len(candidates) != want {
				t.Fatalf("Resolve() returned %d candidates from %d matching Universes; want output bounded at %d", len(candidates), count, MaxUniverseCandidates)
			}
			for index, candidate := range candidates {
				wantID := domain.UniverseID(fmt.Sprintf("universe-%02d", index))
				if candidate.UniverseID != wantID {
					t.Errorf("candidate[%d].UniverseID = %q, want deterministic bounded candidate %q", index, candidate.UniverseID, wantID)
				}
			}
		})
	}
}

func discoveryHit(provider, externalID, title string, medium domain.Medium) domain.MetadataSearchResult {
	workType := map[domain.Medium]string{
		domain.MediumGame: "game", domain.MediumVideo: "series", domain.MediumLiterature: "book", domain.MediumAudio: "music",
	}[medium]
	return domain.MetadataSearchResult{
		Provider: provider, ExternalID: externalID, Title: title, Medium: medium, MediaType: workType, WorkType: workType,
		Relevance: domain.SearchRelevanceBest,
	}
}
