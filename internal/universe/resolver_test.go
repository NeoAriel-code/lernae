package universe

import (
	"reflect"
	"testing"

	"lernae/internal/domain"
)

func TestNormalizeTitleUsesUnicodeCaseFoldAndPunctuationBoundaries(t *testing.T) {
	got := NormalizeTitle("  POKÉMON—Épée:  Version  ")
	want := NormalizeTitle("Poke\u0301mon E\u0301pe\u0301e Version")
	if got != want {
		t.Fatalf("normalized titles differ: %q != %q", got, want)
	}
	if NormalizeTitle("Pokémon") == NormalizeTitle("Pokemon") {
		t.Fatal("normalization erased a semantic accent")
	}
	if NormalizeTitle("Straße") != "strasse" {
		t.Fatalf("Unicode case fold = %q, want %q", NormalizeTitle("Straße"), "strasse")
	}
}

func TestResolveExactTitleAnchorAndPunctuationCase(t *testing.T) {
	candidates := Resolve(ResolveInput{
		Query:   "DRAGON'S CROWN",
		Results: []domain.MetadataSearchResult{result("igdb", "game-1", "Dragon’s Crown", "game")},
	})
	proposal := onlyProposal(t, candidates)
	if proposal.Provider != "igdb" || proposal.ExternalID != "game-1" || proposal.MediaType != "game" || proposal.Title != "Dragon’s Crown" {
		t.Fatalf("proposal lost exact provider identity or display metadata: %#v", proposal)
	}
	if proposal.Evidence != EvidenceExactTitleAnchor || proposal.ExistingState != MembershipNone || !proposal.RequiresConfirmation {
		t.Fatalf("exact title proposal = %#v", proposal)
	}
	if proposal.Confidence <= 0 || proposal.Confidence > 1 {
		t.Fatalf("confidence outside (0,1]: %v", proposal.Confidence)
	}
	if proposal.Reason == "" {
		t.Fatal("proposal omitted its explainable reason")
	}
}

func TestResolveDistinctiveWholeTokenEvidenceGroupsRelatedResults(t *testing.T) {
	candidates := Resolve(ResolveInput{
		Query: "Evangelion",
		Results: []domain.MetadataSearchResult{
			result("igdb", "game-1", "Neon Genesis Evangelion", "game"),
			result("openlibrary", "book-1", "The End of Evangelion", "book"),
			result("tvmaze", "show-1", "Evangelion: Rebuild", "show"),
			result("openlibrary", "unrelated", "A Quiet Place", "book"),
		},
	})
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want one query-anchored candidate: %#v", len(candidates), candidates)
	}
	if got := len(candidates[0].Memberships); got != 3 {
		t.Fatalf("proposal count = %d, want 3 related whole-token matches", got)
	}
	for _, proposal := range candidates[0].Memberships {
		if proposal.Evidence != EvidenceDistinctiveToken || proposal.ExistingState != MembershipNone || !proposal.RequiresConfirmation {
			t.Errorf("unexpected proposal: %#v", proposal)
		}
		if proposal.Confidence != 0.8 || proposal.ConfidenceLevel != ConfidenceModerate {
			t.Errorf("distinctive-token confidence = %v/%q, want bounded moderate rule value", proposal.Confidence, proposal.ConfidenceLevel)
		}
	}
}

func TestResolveIsDeterministicAcrossInputOrdering(t *testing.T) {
	universeA := UniverseState{Universe: domain.Universe{ID: "a", Title: "Neon Genesis Evangelion"}}
	universeB := UniverseState{Universe: domain.Universe{ID: "b", Title: "The End of Evangelion"}}
	first := Resolve(ResolveInput{
		Query: "Evangelion",
		Results: []domain.MetadataSearchResult{
			result("tvmaze", "show-2", "Rebuild of Evangelion", "show"),
			result("openlibrary", "book-1", "The End of Evangelion", "book"),
		},
		Universes: []UniverseState{universeB, universeA},
	})
	second := Resolve(ResolveInput{
		Query: "Evangelion",
		Results: []domain.MetadataSearchResult{
			result("openlibrary", "book-1", "The End of Evangelion", "book"),
			result("tvmaze", "show-2", "Rebuild of Evangelion", "show"),
		},
		Universes: []UniverseState{universeA, universeB},
	})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("input order changed resolver output:\nfirst:  %#v\nsecond: %#v", first, second)
	}
}

func TestResolveDoesNotUseSubstringEvidenceForUnrelatedTitles(t *testing.T) {
	candidates := Resolve(ResolveInput{
		Query: "art",
		Results: []domain.MetadataSearchResult{
			result("openlibrary", "1", "The Art of War", "book"),
			result("openlibrary", "2", "Cartography Basics", "book"),
			result("tvmaze", "3", "Heartland", "show"),
		},
	})
	if len(candidates) != 0 {
		t.Fatalf("short query unexpectedly matched by substring: %#v", candidates)
	}
}

func TestResolveShortAndCommonQueriesDoNotMassGroup(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		titles  []string
		wantMax int
	}{
		{name: "it", query: "It", titles: []string{"It", "It", "It Takes Two", "The It Girl"}, wantMax: 1},
		{name: "up", query: "Up", titles: []string{"Up", "Up", "Up & Away", "The Up Project"}, wantMax: 1},
		{name: "cars", query: "Cars", titles: []string{"Cars", "Cars", "Cars 2", "The Cars Collection"}, wantMax: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := make([]domain.MetadataSearchResult, 0, len(tt.titles))
			for i, title := range tt.titles {
				results = append(results, result("provider", string(rune('a'+i)), title, "work"))
			}
			candidates := Resolve(ResolveInput{
				Query: tt.query, Results: results,
				Universes: []UniverseState{{Universe: domain.Universe{ID: "common", Title: tt.query}}},
			})
			for _, candidate := range candidates {
				if len(candidate.Memberships) > tt.wantMax {
					t.Errorf("common query mass-grouped %d identities: %#v", len(candidate.Memberships), candidate)
				}
			}
		})
	}
}

func TestResolveUsesExplicitUniverseAlias(t *testing.T) {
	state := UniverseState{
		Universe: domain.Universe{ID: "evangelion", Title: "Neon Genesis Evangelion"},
		Aliases:  []domain.UniverseAlias{{UniverseID: "evangelion", Alias: "Evangelion"}},
	}
	candidates := Resolve(ResolveInput{
		Query:     "Evangelion",
		Results:   []domain.MetadataSearchResult{result("tvmaze", "show-1", "The End of Evangelion", "show")},
		Universes: []UniverseState{state},
	})
	proposal := onlyProposal(t, candidates)
	if proposal.UniverseID != "evangelion" || proposal.Evidence != EvidenceExplicitAlias {
		t.Fatalf("alias proposal = %#v", proposal)
	}
}

func TestResolveIgnoresAliasAttachedToAnotherUniverse(t *testing.T) {
	state := UniverseState{
		Universe: domain.Universe{ID: "universe-a", Title: "Unrelated Canonical Title"},
		Aliases:  []domain.UniverseAlias{{UniverseID: "universe-b", Alias: "Lantern"}},
	}
	candidates := Resolve(ResolveInput{
		Query:     "Lantern",
		Results:   []domain.MetadataSearchResult{result("openlibrary", "book-1", "The Lantern", "book")},
		Universes: []UniverseState{state},
	})
	for _, candidate := range candidates {
		if candidate.UniverseID == state.Universe.ID {
			t.Fatalf("alias from another Universe produced a proposal for %q: %#v", state.Universe.ID, candidate)
		}
	}
}

func TestResolveUsesExactConfirmedIdentityAndKeepsMembershipStable(t *testing.T) {
	stateA := UniverseState{Universe: domain.Universe{ID: "series-a", Title: "Series A"}}
	stateB := UniverseState{Universe: domain.Universe{ID: "series-b", Title: "Series B"}}
	membership := domain.UniverseMembership{
		UniverseID: stateA.Universe.ID, WorkID: "work-1", Provenance: domain.UniverseMembershipProvenanceManual,
		Confidence: 0.8, ConfirmedByUser: true,
	}
	candidates := Resolve(ResolveInput{
		Query:   "Series B",
		Results: []domain.MetadataSearchResult{result("tvmaze", "show-1", "Series B", "show")},
		Universes: []UniverseState{
			stateB,
			stateA,
		},
		WorkStates: []WorkIdentityState{{
			Provider: "tvmaze", ExternalID: "show-1", WorkID: "work-1", Membership: &membership,
		}},
	})
	if len(candidates) != 1 || candidates[0].UniverseID != stateA.Universe.ID {
		t.Fatalf("confirmed exact identity moved to a different candidate: %#v", candidates)
	}
	proposal := candidates[0].Memberships[0]
	if proposal.Evidence != EvidenceConfirmedIdentity || proposal.ExistingState != MembershipAccepted || !proposal.ConfirmedByUser {
		t.Fatalf("confirmed exact identity proposal = %#v", proposal)
	}
}

func TestResolveExclusionSuppressesMatchingProposal(t *testing.T) {
	state := UniverseState{Universe: domain.Universe{ID: "world", Title: "The Glass World"}}
	candidates := Resolve(ResolveInput{
		Query:     "Glass World",
		Results:   []domain.MetadataSearchResult{result("openlibrary", "book-1", "The Glass World", "book")},
		Universes: []UniverseState{state},
		WorkStates: []WorkIdentityState{{
			Provider: "openlibrary", ExternalID: "book-1", WorkID: "work-1",
			Exclusions: []domain.UniverseExclusion{{UniverseID: state.Universe.ID, WorkID: "work-1", Reason: "user rejected"}},
		}},
	})
	proposal := onlyProposal(t, candidates)
	if proposal.ExistingState != MembershipExcluded || !proposal.Suppressed || proposal.RequiresConfirmation {
		t.Fatalf("excluded relation was not suppressed: %#v", proposal)
	}
	if candidates[0].Evidence != EvidenceExplicitExclusion || candidates[0].Reason == "" || candidates[0].Confidence != 0 {
		t.Fatalf("excluded candidate explanation = %#v", candidates[0])
	}
}

func TestResolveSkipsAmbiguousExactIdentityState(t *testing.T) {
	state := UniverseState{Universe: domain.Universe{ID: "evangelion", Title: "Neon Genesis Evangelion"}}
	result := result("openlibrary", "book-1", "The End of Evangelion", "book")
	candidates := Resolve(ResolveInput{
		Query:     "Evangelion",
		Results:   []domain.MetadataSearchResult{result},
		Universes: []UniverseState{state},
		WorkStates: []WorkIdentityState{
			{Provider: "openlibrary", ExternalID: "book-1", WorkID: "work-1"},
			{
				Provider: "openlibrary", ExternalID: "book-1", WorkID: "work-1",
				Exclusions: []domain.UniverseExclusion{{UniverseID: state.Universe.ID, WorkID: "work-1", Reason: "user rejected"}},
			},
		},
	})
	if len(candidates) != 0 {
		t.Fatalf("ambiguous identity snapshot was treated as no state: %#v", candidates)
	}
}

func TestResolveSkipsMembershipAttachedToDifferentWorkID(t *testing.T) {
	state := UniverseState{Universe: domain.Universe{ID: "series-a", Title: "Series A"}}
	membership := domain.UniverseMembership{
		UniverseID: state.Universe.ID, WorkID: "work-a", Provenance: domain.UniverseMembershipProvenanceManual,
		ConfirmedByUser: true,
	}
	candidates := Resolve(ResolveInput{
		Query:     "Series B",
		Results:   []domain.MetadataSearchResult{result("tvmaze", "show-1", "Series B", "show")},
		Universes: []UniverseState{state, {Universe: domain.Universe{ID: "series-b", Title: "Series B"}}},
		WorkStates: []WorkIdentityState{{
			Provider: "tvmaze", ExternalID: "show-1", WorkID: "work-b", Membership: &membership,
		}},
	})
	if len(candidates) != 0 {
		t.Fatalf("membership for another Work ID influenced exact identity resolution: %#v", candidates)
	}
}

func TestResolvePreservesSameTitleWithDifferentProviderIdentities(t *testing.T) {
	candidates := Resolve(ResolveInput{
		Query: "Evangelion",
		Results: []domain.MetadataSearchResult{
			result("openlibrary", "book-1", "Evangelion", "book"),
			result("tvmaze", "show-9", "Evangelion", "show"),
		},
	})
	if len(candidates) != 1 || len(candidates[0].Memberships) != 2 {
		t.Fatalf("same-title results were discarded or split unexpectedly: %#v", candidates)
	}
	first, second := candidates[0].Memberships[0], candidates[0].Memberships[1]
	if first.Provider == second.Provider && first.ExternalID == second.ExternalID {
		t.Fatalf("distinct external identities were conflated: %#v", candidates[0].Memberships)
	}
}

func onlyProposal(t *testing.T, candidates []UniverseCandidate) MembershipProposal {
	t.Helper()
	if len(candidates) != 1 || len(candidates[0].Memberships) != 1 {
		t.Fatalf("got %d candidates; want exactly one candidate with one membership proposal: %#v", len(candidates), candidates)
	}
	return candidates[0].Memberships[0]
}

func result(provider, externalID, title, mediaType string) domain.MetadataSearchResult {
	return domain.MetadataSearchResult{
		Provider: provider, ExternalID: externalID, Title: title,
		MediaType: mediaType, Medium: domain.MediumLiterature, WorkType: mediaType,
	}
}
