package search

import (
	"context"
	"strings"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

// MetadataAdapter lets an existing SearchWorks integration participate in the
// limit-aware universal-search contract without changing its Details API.
type MetadataAdapter struct {
	Provider string
	Source   providers.MetadataSource
}

func (adapter MetadataAdapter) Name() string {
	return strings.ToLower(strings.TrimSpace(adapter.Provider))
}

func (adapter MetadataAdapter) Search(ctx context.Context, query string, limit int) ([]domain.MetadataSearchResult, error) {
	if adapter.Source == nil {
		return nil, providers.ErrSearchUnavailable
	}
	results, err := adapter.Source.SearchWorks(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(results) > limit {
		results = results[:limit]
	}
	for index := range results {
		results[index].Provider = adapter.Name()
		results[index].SourceRank = index + 1
		if results[index].MediaType == "" {
			results[index].MediaType = mediaType(results[index])
		}
		if results[index].Year == 0 {
			results[index].Year = results[index].ReleaseYear
		}
		if results[index].ArtworkURL == "" {
			results[index].ArtworkURL = results[index].CoverReference
		}
	}
	return results, nil
}
