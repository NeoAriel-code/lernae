package search

import (
	"testing"

	"lernae/internal/domain"
)

func TestNormalizeSearchTextUsesCompatibilityCaseFoldingAndTokenBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "compatibility forms and combining accents", input: "  ＡＢＣ—Cafe\u0301! ", want: "abc café"},
		{name: "full case fold and non-latin tokens", input: "Straße / 東京—物語", want: "strasse 東京 物語"},
		{name: "punctuation separates words", input: "Neon-Genesis: Evangelion", want: "neon genesis evangelion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeSearchText(test.input); got != test.want {
				t.Fatalf("normalizeSearchText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestClassifySearchRelevanceUsesTitlePhrasesAndExplicitAliases(t *testing.T) {
	for _, test := range []struct {
		name   string
		query  string
		result domain.MetadataSearchResult
		want   domain.SearchRelevance
	}{
		{
			name:   "exact normalized title",
			query:  "café—noir",
			result: domain.MetadataSearchResult{Title: "Cafe\u0301 Noir"},
			want:   domain.SearchRelevanceBest,
		},
		{
			name:   "whole distinctive phrase",
			query:  "neon genesis evangelion",
			result: domain.MetadataSearchResult{Title: "Neon Genesis Evangelion: The End"},
			want:   domain.SearchRelevanceBest,
		},
		{
			name:   "franchise phrase after unrelated title prefix is not best",
			query:  "Game of Thrones",
			result: domain.MetadataSearchResult{Title: "Hip Hop Tribe 2 : Game of Thrones"},
			want:   domain.SearchRelevanceWeak,
		},
		{
			name:   "franchise-leading phrase remains best",
			query:  "Game of Thrones",
			result: domain.MetadataSearchResult{Title: "Game of Thrones: A Telltale Games Series"},
			want:   domain.SearchRelevanceBest,
		},
		{
			name:   "franchise-leading phrase after an article remains best",
			query:  "Game of Thrones",
			result: domain.MetadataSearchResult{Title: "A Game of Thrones"},
			want:   domain.SearchRelevanceBest,
		},
		{
			name:   "explicit provider alias",
			query:  "case closed",
			result: domain.MetadataSearchResult{Title: "Detective Conan", Aliases: []string{"Case Closed"}},
			want:   domain.SearchRelevanceBest,
		},
		{
			name:   "related shared title anchors",
			query:  "wheel of time",
			result: domain.MetadataSearchResult{Title: "Time Beyond the Wheel"},
			want:   domain.SearchRelevanceRelated,
		},
		{
			name:   "Evangelion is not a substring match for Evangelical",
			query:  "evangelion",
			result: domain.MetadataSearchResult{Title: "Evangelical Treatise"},
			want:   domain.SearchRelevanceWeak,
		},
		{
			name:   "single shared Conan token is weak",
			query:  "detective conan",
			result: domain.MetadataSearchResult{Title: "Conan Doyle's Sherlock Holmes"},
			want:   domain.SearchRelevanceWeak,
		},
		{
			name:  "creator and summary collisions are weak",
			query: "detective conan",
			result: domain.MetadataSearchResult{
				Title: "Sherlock Holmes", Creators: []string{"Arthur Conan Doyle"},
				Summary: "A detective story that mentions Conan.",
			},
			want: domain.SearchRelevanceWeak,
		},
		{
			name:   "generic token overlap is weak",
			query:  "game",
			result: domain.MetadataSearchResult{Title: "Game Studio Collection"},
			want:   domain.SearchRelevanceWeak,
		},
		{
			name:   "Evangelion religious phrase is not best",
			query:  "evangelion",
			result: domain.MetadataSearchResult{Title: "The Gospel of Evangelion"},
			want:   domain.SearchRelevanceWeak,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySearchRelevance(test.query, test.result); got != test.want {
				t.Fatalf("classifySearchRelevance(%q, %q) = %q, want %q", test.query, test.result.Title, got, test.want)
			}
		})
	}
}

func TestClassifySearchRelevanceUsesCompactCanonicalTitlesAndAliases(t *testing.T) {
	for _, test := range []struct {
		name   string
		query  string
		result domain.MetadataSearchResult
	}{
		{
			name:   "case and compact provider title",
			query:  "soul calibur",
			result: domain.MetadataSearchResult{Title: "SOULCALIBUR"},
		},
		{
			name:   "punctuation and spacing provider title",
			query:  "Soul-Calibur",
			result: domain.MetadataSearchResult{Title: "Soul Calibur"},
		},
		{
			name:   "unrelated franchise is normalized generically",
			query:  "Mystery Quest",
			result: domain.MetadataSearchResult{Title: "MysteryQuest"},
		},
		{
			name:  "exact canonical provider alias",
			query: "CASE CLOSED",
			result: domain.MetadataSearchResult{
				Title: "Detective Conan", Aliases: []string{"CaseClosed"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySearchRelevance(test.query, test.result); got != domain.SearchRelevanceBest {
				t.Fatalf("classifySearchRelevance(%q, %#v) = %q, want %q", test.query, test.result, got, domain.SearchRelevanceBest)
			}
		})
	}
}

func TestClassifySearchRelevanceKeepsGenericGameQueryWeak(t *testing.T) {
	result := domain.MetadataSearchResult{Title: "Game", Aliases: []string{"Games"}}
	if got := classifySearchRelevance("game", result); got != domain.SearchRelevanceWeak {
		t.Fatalf("classifySearchRelevance(%q, %#v) = %q, want %q", "game", result, got, domain.SearchRelevanceWeak)
	}
}

func TestClassifySearchRelevanceRanksGameInstallmentsAboveGuideCollisions(t *testing.T) {
	for _, test := range []struct {
		name   string
		result domain.MetadataSearchResult
		want   domain.SearchRelevance
	}{
		{
			name: "compact game installment",
			result: domain.MetadataSearchResult{
				Title: "SoulCalibur II", Medium: domain.MediumGame, MediaType: "game", WorkType: "game",
			},
			want: domain.SearchRelevanceBest,
		},
		{
			name: "spaced game installment",
			result: domain.MetadataSearchResult{
				Title: "Soul Calibur VI", Medium: domain.MediumGame, MediaType: "game", WorkType: "game",
			},
			want: domain.SearchRelevanceBest,
		},
		{
			name: "guide sharing installment title",
			result: domain.MetadataSearchResult{
				Title: "Soul Calibur II Official Strategy Guide", Medium: domain.MediumLiterature,
				Aliases: []string{"SOULCALIBUR"}, MediaType: "book", WorkType: "book",
			},
			want: domain.SearchRelevanceRelated,
		},
		{
			name: "game-typed guide is not promoted",
			result: domain.MetadataSearchResult{
				Title: "Soul Calibur II Official Strategy Guide", WorkType: "game",
			},
			want: domain.SearchRelevanceRelated,
		},
		{
			name: "embedded phrase is only a related collision",
			result: domain.MetadataSearchResult{
				Title: "The Soul Calibur Mystery", Medium: domain.MediumLiterature,
				MediaType: "book", WorkType: "book",
			},
			want: domain.SearchRelevanceRelated,
		},
		{
			name: "compact substring collision is not a match",
			result: domain.MetadataSearchResult{
				Title: "SoulCaliburn II", Medium: domain.MediumGame, MediaType: "game", WorkType: "game",
			},
			want: domain.SearchRelevanceWeak,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySearchRelevance("soul calibur", test.result); got != test.want {
				t.Fatalf("classifySearchRelevance(%q, %q) = %q, want %q", "soul calibur", test.result.Title, got, test.want)
			}
		})
	}
}
