package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

type providerStub struct {
	name    string
	results []domain.MetadataSearchResult
	err     error
	started chan struct{}
	wait    bool
}

func (provider providerStub) Name() string { return provider.name }

func (provider providerStub) Search(ctx context.Context, _ string, limit int) ([]domain.MetadataSearchResult, error) {
	if provider.started != nil {
		select {
		case provider.started <- struct{}{}:
		default:
		}
	}
	if provider.err != nil {
		return provider.results, provider.err
	}
	if provider.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if len(provider.results) > limit {
		return provider.results[:limit], nil
	}
	return provider.results, nil
}

func hit(provider, id string, score float64) domain.MetadataSearchResult {
	return domain.MetadataSearchResult{
		Provider: provider, ExternalID: id, Title: id, Medium: domain.MediumLiterature,
		WorkType: "book", MediaType: "book", Score: &score,
	}
}

func TestAggregatorRoundRobinsProviderLocalRankingsWithoutComparingScores(t *testing.T) {
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second},
		providerStub{name: "igdb", results: []domain.MetadataSearchResult{hit("igdb", "g1", 0.01), hit("igdb", "g2", 1000)}},
		providerStub{name: "openlibrary", results: []domain.MetadataSearchResult{hit("openlibrary", "b1", 10000), hit("openlibrary", "b2", 0.001)}},
		providerStub{name: "tvmaze", results: []domain.MetadataSearchResult{hit("tvmaze", "t1", 9)}},
	)

	response, err := aggregator.Search(context.Background(), "Wheel of Time", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []string{"igdb:g1", "openlibrary:b1", "tvmaze:t1", "igdb:g2", "openlibrary:b2"}
	if len(response.Results) != len(want) {
		t.Fatalf("result count = %d, want %d: %#v", len(response.Results), len(want), response.Results)
	}
	for index, result := range response.Results {
		if got := result.Provider + ":" + result.ExternalID; got != want[index] {
			t.Errorf("result[%d] identity = %q, want %q", index, got, want[index])
		}
		if result.SourceRank == 0 {
			t.Errorf("result[%d] has no provider-local source rank", index)
		}
	}
	if len(response.Sources) != 3 || response.Sources[0].Provider != "igdb" || response.Sources[1].Provider != "openlibrary" || response.Sources[2].Provider != "tvmaze" {
		t.Fatalf("source order = %#v, want stable configured order", response.Sources)
	}
}

func TestAggregatorOrdersRelevanceBandsBeforeStableRoundRobin(t *testing.T) {
	result := func(provider, id, title string, score float64) domain.MetadataSearchResult {
		return domain.MetadataSearchResult{
			Provider: provider, ExternalID: id, Title: title, Medium: domain.MediumLiterature,
			WorkType: "book", MediaType: "book", Score: &score,
		}
	}
	caseClosed := result("igdb", "best-2", "Case Closed: Detective Conan", 0.03)
	caseClosed.Aliases = []string{"Case Closed"}
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second},
		providerStub{name: "igdb", results: []domain.MetadataSearchResult{
			result("igdb", "best-1", "Detective Conan", 0.01),
			result("igdb", "related-1", "The Conan and Detective", 0.02),
			result("igdb", "weak-1", "Sherlock Holmes", 10000),
			caseClosed,
		}},
		providerStub{name: "openlibrary", results: []domain.MetadataSearchResult{
			result("openlibrary", "related-2", "Detective Holmes Meets Conan", 10000),
			result("openlibrary", "best-3", "Detective Conan: The Movie", 0.001),
			result("openlibrary", "weak-2", "Conan Doyle's Sherlock Holmes", 100000),
		}},
	)

	response, err := aggregator.Search(context.Background(), "Detective Conan", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []struct {
		identity  string
		relevance domain.SearchRelevance
	}{
		{identity: "igdb:best-1", relevance: domain.SearchRelevanceBest},
		{identity: "openlibrary:best-3", relevance: domain.SearchRelevanceBest},
		{identity: "igdb:best-2", relevance: domain.SearchRelevanceBest},
		{identity: "igdb:related-1", relevance: domain.SearchRelevanceRelated},
		{identity: "openlibrary:related-2", relevance: domain.SearchRelevanceRelated},
		{identity: "igdb:weak-1", relevance: domain.SearchRelevanceWeak},
		{identity: "openlibrary:weak-2", relevance: domain.SearchRelevanceWeak},
	}
	if len(response.Results) != len(want) {
		t.Fatalf("result count = %d, want %d: %#v", len(response.Results), len(want), response.Results)
	}
	for index, expected := range want {
		got := response.Results[index]
		if identity := got.Provider + ":" + got.ExternalID; identity != expected.identity {
			t.Errorf("result[%d] identity = %q, want %q", index, identity, expected.identity)
		}
		if got.Relevance != expected.relevance {
			t.Errorf("result[%d] relevance = %q, want %q", index, got.Relevance, expected.relevance)
		}
	}
	if got := response.Results[2].SourceRank; got != 4 {
		t.Errorf("second IGDB Best result source rank = %d, want original provider-local rank 4", got)
	}
}

func TestAggregatorPrioritizesCoreCompactFranchiseGamesOverGuidesAndCollisions(t *testing.T) {
	result := func(provider, id, title string, medium domain.Medium, mediaType, workType string) domain.MetadataSearchResult {
		return domain.MetadataSearchResult{
			Provider: provider, ExternalID: id, Title: title, Medium: medium, MediaType: mediaType, WorkType: workType,
		}
	}
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second},
		providerStub{name: "openlibrary", results: []domain.MetadataSearchResult{
			result("openlibrary", "exact-book", "Soul Calibur", domain.MediumLiterature, "book", "book"),
			result("openlibrary", "strategy-guide", "Soul Calibur II Official Strategy Guide", domain.MediumLiterature, "book", "book"),
			result("openlibrary", "phrase-collision", "The Soul Calibur Mystery", domain.MediumLiterature, "book", "book"),
		}},
		providerStub{name: "igdb", results: []domain.MetadataSearchResult{
			result("igdb", "soulcalibur-vi", "SOULCALIBUR VI", domain.MediumGame, "game", "game"),
			result("igdb", "soulcalibur-ii", "Soul Calibur II", domain.MediumGame, "game", "game"),
			result("igdb", "substring", "SoulCaliburn II", domain.MediumGame, "game", "game"),
		}},
	)

	response, err := aggregator.Search(context.Background(), "soul calibur", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []struct {
		id        string
		relevance domain.SearchRelevance
	}{
		{id: "soulcalibur-vi", relevance: domain.SearchRelevanceBest},
		{id: "soulcalibur-ii", relevance: domain.SearchRelevanceBest},
		{id: "exact-book", relevance: domain.SearchRelevanceBest},
		{id: "strategy-guide", relevance: domain.SearchRelevanceRelated},
		{id: "phrase-collision", relevance: domain.SearchRelevanceRelated},
		{id: "substring", relevance: domain.SearchRelevanceWeak},
	}
	if len(response.Results) != len(want) {
		t.Fatalf("result count = %d, want %d: %#v", len(response.Results), len(want), response.Results)
	}
	for index, expected := range want {
		got := response.Results[index]
		if got.ExternalID != expected.id || got.Relevance != expected.relevance {
			t.Errorf("result[%d] = %s/%s, want %s/%s", index, got.ExternalID, got.Relevance, expected.id, expected.relevance)
		}
	}
}

func TestAggregatorReturnsPartialResultsAndSafeProviderState(t *testing.T) {
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second},
		providerStub{name: "igdb", results: []domain.MetadataSearchResult{hit("igdb", "game-1", 1)}},
		providerStub{name: "openlibrary", err: errors.New("private provider response body and credential")},
	)
	response, err := aggregator.Search(context.Background(), "Soulcalibur", 20)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].ExternalID != "game-1" {
		t.Fatalf("partial results = %#v, want successful IGDB result", response.Results)
	}
	if got := response.Sources[1].State; got != domain.SearchSourceUnavailable {
		t.Fatalf("failed source state = %q, want unavailable", got)
	}
	if strings.Contains(fmt.Sprint(response), "private provider") || strings.Contains(fmt.Sprint(response), "credential") {
		t.Fatalf("private provider detail leaked into public response: %#v", response)
	}
}

func TestAggregatorRetainsCachedStaleResultsAndMarksSource(t *testing.T) {
	stale := hit("openlibrary", "OL1W", 1)
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second},
		providerStub{name: "openlibrary", results: []domain.MetadataSearchResult{stale}, err: providers.ErrSearchStale},
	)
	response, err := aggregator.Search(context.Background(), "Wheel of Time", 20)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].ExternalID != "OL1W" {
		t.Fatalf("stale result = %#v, want cached book", response.Results)
	}
	if response.Sources[0].State != domain.SearchSourceStale || response.Sources[0].ErrorCode != "stale_cache" {
		t.Fatalf("stale source status = %#v", response.Sources[0])
	}
}

type staleAfterDeadlineProvider struct {
	result domain.MetadataSearchResult
}

func (staleAfterDeadlineProvider) Name() string { return "openlibrary" }

func (provider staleAfterDeadlineProvider) Search(ctx context.Context, _ string, _ int) ([]domain.MetadataSearchResult, error) {
	<-ctx.Done()
	time.Sleep(10 * time.Millisecond)
	return []domain.MetadataSearchResult{provider.result}, providers.ErrSearchStale
}

func TestAggregatorRetainsStaleSnapshotReturnedJustAfterProviderDeadline(t *testing.T) {
	aggregator := NewAggregator(Config{OverallTimeout: 250 * time.Millisecond, ProviderTimeout: 20 * time.Millisecond},
		staleAfterDeadlineProvider{result: hit("openlibrary", "OL1W", 1)},
	)

	response, err := aggregator.Search(context.Background(), "Wheel of Time", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].ExternalID != "OL1W" {
		t.Fatalf("stale result = %#v, want cached book", response.Results)
	}
	if response.Sources[0].State != domain.SearchSourceStale {
		t.Fatalf("source status = %#v, want stale", response.Sources[0])
	}
}

func TestAggregatorBoundsQueryAndResultLimit(t *testing.T) {
	provider := providerStub{name: "igdb", results: []domain.MetadataSearchResult{
		hit("igdb", "1", 1), hit("igdb", "2", 2), hit("igdb", "3", 3),
	}}
	for _, test := range []struct {
		name  string
		query string
		limit int
	}{
		{name: "empty query", query: "  ", limit: 1},
		{name: "query exceeds byte bound", query: strings.Repeat("x", MaxQueryBytes+1), limit: 1},
		{name: "zero limit", query: "title", limit: 0},
		{name: "limit exceeds provider bound", query: "title", limit: MaxProviderResults + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second}, provider)
			if _, err := aggregator.Search(context.Background(), test.query, test.limit); !errors.Is(err, ErrInvalidSearch) {
				t.Fatalf("Search() error = %v, want ErrInvalidSearch", err)
			}
		})
	}
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: time.Second}, provider)
	response, err := aggregator.Search(context.Background(), "title", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("result count = %d, want bounded 2", len(response.Results))
	}
}

func TestAggregatorHonorsProviderTimeoutAndParentCancellation(t *testing.T) {
	provider := providerStub{name: "igdb", started: make(chan struct{}, 1), wait: true}
	aggregator := NewAggregator(Config{OverallTimeout: time.Second, ProviderTimeout: 20 * time.Millisecond}, provider)
	response, err := aggregator.Search(context.Background(), "title", 1)
	if err != nil {
		t.Fatalf("provider timeout should be represented as source state, got error %v", err)
	}
	if response.Sources[0].State != domain.SearchSourceUnavailable {
		t.Fatalf("timed-out source state = %q, want unavailable", response.Sources[0].State)
	}
	select {
	case <-provider.started:
	default:
		t.Fatal("provider was not invoked")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := aggregator.Search(ctx, "title", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error = %v, want context.Canceled", err)
	}
}
