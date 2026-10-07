package providers

import (
	"context"

	"lernae/internal/domain"
)

// WorkDetailsRouter dispatches a canonical external identity to exactly one
// registered provider. Provider names are matched exactly; adapters validate
// their own external ID syntax.
type WorkDetailsRouter struct {
	sources map[string]WorkDetailsSource
}

func NewWorkDetailsRouter(sources map[string]WorkDetailsSource) *WorkDetailsRouter {
	registered := make(map[string]WorkDetailsSource, len(sources))
	for provider, source := range sources {
		if provider != "" && source != nil {
			registered[provider] = source
		}
	}
	return &WorkDetailsRouter{sources: registered}
}

func (router *WorkDetailsRouter) GetWorkDetails(ctx context.Context, provider, externalID string) (domain.WorkDetails, error) {
	if router == nil {
		return domain.WorkDetails{}, ErrDetailsUnsupportedProvider
	}
	source := router.sources[provider]
	if source == nil {
		return domain.WorkDetails{}, ErrDetailsUnsupportedProvider
	}
	return source.GetWorkDetails(ctx, provider, externalID)
}
