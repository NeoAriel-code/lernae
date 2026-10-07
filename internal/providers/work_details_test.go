package providers

import (
	"context"
	"errors"
	"testing"

	"lernae/internal/domain"
)

type workDetailsProviderStub struct {
	provider string
	calls    int
}

func (source *workDetailsProviderStub) GetWorkDetails(_ context.Context, provider, externalID string) (domain.WorkDetails, error) {
	source.calls++
	return domain.WorkDetails{Provider: source.provider, ExternalID: externalID, Title: provider}, nil
}

func TestWorkDetailsRouterDispatchesOnlyExactProviderIdentity(t *testing.T) {
	books := &workDetailsProviderStub{provider: "openlibrary"}
	shows := &workDetailsProviderStub{provider: "tvmaze"}
	router := NewWorkDetailsRouter(map[string]WorkDetailsSource{"openlibrary": books, "tvmaze": shows})
	details, err := router.GetWorkDetails(context.Background(), "openlibrary", "OL123W")
	if err != nil || details.Provider != "openlibrary" || details.ExternalID != "OL123W" || books.calls != 1 || shows.calls != 0 {
		t.Fatalf("Open Library dispatch = %#v, %v; calls=%d/%d", details, err, books.calls, shows.calls)
	}
	if _, err := router.GetWorkDetails(context.Background(), "OpenLibrary", "OL123W"); !errors.Is(err, ErrDetailsUnsupportedProvider) {
		t.Fatalf("case-folded provider error = %v, want unsupported provider", err)
	}
	if books.calls != 1 || shows.calls != 0 {
		t.Fatalf("unsupported provider dispatch changed calls=%d/%d", books.calls, shows.calls)
	}
}
