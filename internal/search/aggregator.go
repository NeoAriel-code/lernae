// Package search provides bounded, provider-neutral search aggregation.
package search

import (
	"context"
	"errors"
	"strings"
	"time"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	MaxProviderResults = providers.MaxProviderSearchResults
	MaxQueryBytes      = providers.MaxUniversalSearchQueryBytes
	defaultTimeout     = 5 * time.Second
	// Leave room for one TVMaze 429 backoff and its retry while preserving the
	// overall request timeout as the final bound.
	providerTimeout     = 4 * time.Second
	providerResultGrace = 25 * time.Millisecond
)

var ErrInvalidSearch = providers.ErrSearchInvalid

type Config struct {
	OverallTimeout  time.Duration
	ProviderTimeout time.Duration
}

type Aggregator struct {
	providers       []providers.SearchProvider
	overallTimeout  time.Duration
	providerTimeout time.Duration
}

func NewAggregator(config Config, sources ...providers.SearchProvider) *Aggregator {
	if config.OverallTimeout <= 0 {
		config.OverallTimeout = defaultTimeout
	}
	if config.ProviderTimeout <= 0 {
		config.ProviderTimeout = providerTimeout
	}
	return &Aggregator{
		providers:       append([]providers.SearchProvider(nil), sources...),
		overallTimeout:  config.OverallTimeout,
		providerTimeout: config.ProviderTimeout,
	}
}

type providerResult struct {
	index    int
	results  []domain.MetadataSearchResult
	err      error
	timedOut bool
}

// Search runs providers concurrently, retains successful sources when others
// fail, groups results by Lernae relevance, and round-robins each band in
// configured provider order without comparing provider score scales.
func (aggregator *Aggregator) Search(ctx context.Context, query string, limit int) (domain.SearchResponse, error) {
	if err := validateSearch(query, limit); err != nil {
		return domain.SearchResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.SearchResponse{}, err
	}
	query = strings.TrimSpace(query)
	response := domain.SearchResponse{
		Results: []domain.MetadataSearchResult{},
		Sources: make([]domain.SearchSourceStatus, len(aggregator.providers)),
	}
	if len(aggregator.providers) == 0 {
		return response, nil
	}

	requestCtx, cancelRequest := context.WithTimeout(ctx, aggregator.overallTimeout)
	defer cancelRequest()
	completed := make(chan providerResult, len(aggregator.providers))
	for index, provider := range aggregator.providers {
		name := "unknown"
		if provider != nil && strings.TrimSpace(provider.Name()) != "" {
			name = strings.ToLower(strings.TrimSpace(provider.Name()))
		}
		response.Sources[index] = domain.SearchSourceStatus{Provider: name, State: domain.SearchSourceUnavailable, ErrorCode: "provider_unavailable"}
		go func(index int, provider providers.SearchProvider) {
			if provider == nil {
				completed <- providerResult{index: index, err: errors.New("provider unavailable")}
				return
			}
			providerCtx, cancelProvider := context.WithTimeout(requestCtx, aggregator.providerTimeout)
			defer cancelProvider()
			finished := make(chan providerResult, 1)
			go func() {
				results, err := provider.Search(providerCtx, query, limit)
				finished <- providerResult{index: index, results: results, err: err}
			}()
			select {
			case result := <-finished:
				completed <- result
			case <-providerCtx.Done():
				// A cache may return a safe stale snapshot just after its
				// provider request receives the deadline. Give that already-bound
				// call a tiny bounded window to publish its result.
				timer := time.NewTimer(providerResultGrace)
				defer timer.Stop()
				select {
				case result := <-finished:
					completed <- result
				case <-timer.C:
					completed <- providerResult{index: index, err: providerCtx.Err(), timedOut: errors.Is(providerCtx.Err(), context.DeadlineExceeded)}
				}
			}
		}(index, provider)
	}

	lists := make([][]domain.MetadataSearchResult, len(aggregator.providers))
	for range aggregator.providers {
		select {
		case result := <-completed:
			name := response.Sources[result.index].Provider
			stale := errors.Is(result.err, providers.ErrSearchStale)
			if result.err != nil && !stale {
				if errors.Is(result.err, providers.ErrSearchThrottled) {
					response.Sources[result.index] = domain.SearchSourceStatus{Provider: name, State: domain.SearchSourceThrottled, ErrorCode: "provider_throttled"}
				} else if result.timedOut || errors.Is(result.err, context.DeadlineExceeded) {
					response.Sources[result.index] = domain.SearchSourceStatus{Provider: name, State: domain.SearchSourceUnavailable, ErrorCode: "provider_timeout"}
				} else {
					response.Sources[result.index] = domain.SearchSourceStatus{Provider: name, State: domain.SearchSourceUnavailable, ErrorCode: "provider_unavailable"}
				}
				continue
			}
			for _, item := range result.results {
				if len(lists[result.index]) >= limit {
					break
				}
				if item.Provider != name || strings.TrimSpace(item.ExternalID) == "" || strings.TrimSpace(item.Title) == "" {
					continue
				}
				if item.MediaType == "" {
					item.MediaType = mediaType(item)
				}
				if item.Year == 0 {
					item.Year = item.ReleaseYear
				}
				if item.ArtworkURL == "" {
					item.ArtworkURL = item.CoverReference
				}
				item.SourceRank = len(lists[result.index]) + 1
				lists[result.index] = append(lists[result.index], item)
			}
			state := domain.SearchSourceAvailable
			if len(lists[result.index]) == 0 {
				state = domain.SearchSourceEmpty
			}
			sourceStatus := domain.SearchSourceStatus{Provider: name, State: state, ResultCount: len(lists[result.index])}
			if stale {
				sourceStatus.State = domain.SearchSourceStale
				sourceStatus.ErrorCode = "stale_cache"
			}
			response.Sources[result.index] = sourceStatus
		case <-requestCtx.Done():
			if err := ctx.Err(); err != nil {
				return domain.SearchResponse{}, err
			}
			// Preserve any source state already observed and mark only the
			// providers still awaiting completion as timed out.
			for index, source := range response.Sources {
				if source.ResultCount == 0 && source.State == domain.SearchSourceUnavailable && source.ErrorCode == "provider_unavailable" {
					response.Sources[index].ErrorCode = "provider_timeout"
				}
			}
			response.Results = rankAndRoundRobin(query, lists)
			return response, nil
		}
	}
	response.Results = rankAndRoundRobin(query, lists)
	return response, nil
}

func validateSearch(query string, limit int) error {
	return providers.ValidateSearchRequest(query, limit)
}

func roundRobin(lists [][]domain.MetadataSearchResult) []domain.MetadataSearchResult {
	count := 0
	for _, list := range lists {
		count += len(list)
	}
	results := make([]domain.MetadataSearchResult, 0, count)
	for rank := 0; ; rank++ {
		added := false
		for _, list := range lists {
			if rank < len(list) {
				results = append(results, list[rank])
				added = true
			}
		}
		if !added {
			return results
		}
	}
}

func rankAndRoundRobin(query string, lists [][]domain.MetadataSearchResult) []domain.MetadataSearchResult {
	bands := [4][][]domain.MetadataSearchResult{
		make([][]domain.MetadataSearchResult, len(lists)),
		make([][]domain.MetadataSearchResult, len(lists)),
		make([][]domain.MetadataSearchResult, len(lists)),
		make([][]domain.MetadataSearchResult, len(lists)),
	}
	for providerIndex, list := range lists {
		for _, result := range list {
			result.Relevance = classifySearchRelevance(query, result)
			band := 3
			switch result.Relevance {
			case domain.SearchRelevanceBest:
				band = 1
				if isCoreGameSearchMatch(query, result) {
					band = 0
				}
			case domain.SearchRelevanceRelated:
				band = 2
			}
			bands[band][providerIndex] = append(bands[band][providerIndex], result)
		}
	}

	results := make([]domain.MetadataSearchResult, 0)
	for _, band := range bands {
		results = append(results, roundRobin(band)...)
	}
	return results
}

func mediaType(result domain.MetadataSearchResult) string {
	switch result.Medium {
	case domain.MediumGame:
		return "game"
	case domain.MediumLiterature:
		return "book"
	case domain.MediumVideo:
		return "tv"
	case domain.MediumAudio:
		return "music"
	default:
		return result.WorkType
	}
}
