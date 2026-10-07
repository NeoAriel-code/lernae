package openlibrary

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type immediateLimiter struct{}

func (immediateLimiter) Wait(context.Context) error { return nil }

func TestSearchCacheIdentityTracksMetadataLanguagePreference(t *testing.T) {
	source := New(ClientConfig{MetadataLanguage: "en"})
	if got := source.SearchCacheIdentity(); got != "metadata-language-en" {
		t.Fatalf("default search cache identity = %q, want metadata-language-en", got)
	}
	for _, language := range []string{"es", "original"} {
		source.SetMetadataLanguage(language)
		if got, want := source.SearchCacheIdentity(), "metadata-language-"+language; got != want {
			t.Fatalf("search cache identity for %q = %q, want %q", language, got, want)
		}
	}
}

func TestSearchRequestsExplicitFieldsAndMapsWorksInProviderOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/search.json" {
			t.Errorf("request = %s %s, want GET /search.json", request.Method, request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("q") != "Wheel of Time" || query.Get("limit") != "4" ||
			query.Has("lang") ||
			query.Get("fields") != "key,title,author_name,first_publish_year,cover_i,editions.key,editions.title,editions.language,editions.publish_date,editions.cover_i" {
			t.Errorf("query parameters = %#v", query)
		}
		if request.Header.Get("User-Agent") == "" || strings.Contains(strings.ToLower(request.Header.Get("User-Agent")), "@") {
			t.Errorf("User-Agent = %q, want app identity without invented contact", request.Header.Get("User-Agent"))
		}
		_, _ = io.WriteString(w, `{"docs":[{"key":"/works/OL123W","title":"The Eye of the World","author_name":["Robert Jordan"],"first_publish_year":1990,"cover_i":42,"editions":{"docs":[{"key":"/books/OL321M","title":"The Eye of the World","language":["eng"],"publish_date":"1990","cover_i":99}]}},{"key":"/works/OL124W","title":"The Great Hunt","author_name":["Robert Jordan"],"first_publish_year":1991}]}`)
	}))
	defer server.Close()

	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	results, err := source.Search(context.Background(), "Wheel of Time", 4)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	first := results[0]
	if first.Provider != "openlibrary" || first.ExternalID != "OL123W" || first.Title != "The Eye of the World" || first.MediaType != "book" || first.Medium != "literature" || first.WorkType != "book" {
		t.Fatalf("normalized first result = %#v", first)
	}
	if first.Year != 1990 || first.ReleaseYear != 1990 || len(first.Creators) != 1 || first.Creators[0] != "Robert Jordan" {
		t.Fatalf("normalized book metadata = %#v", first)
	}
	wantCover := "https://covers.openlibrary.org/b/id/99-M.jpg"
	if first.ArtworkURL != wantCover || first.CoverReference != wantCover || first.SourceURL != "https://openlibrary.org/works/OL123W" || first.SourceRank != 1 {
		t.Fatalf("provider provenance/artwork = %#v", first)
	}
	if first.RepresentativeEdition == nil || first.RepresentativeEdition.ExternalID != "OL321M" ||
		first.RepresentativeEdition.Title != "The Eye of the World" || first.RepresentativeEdition.Language != "en" ||
		first.RepresentativeEdition.PublicationDate != "1990" ||
		first.RepresentativeEdition.CoverReference != "https://covers.openlibrary.org/b/id/99-M.jpg" {
		t.Fatalf("representative edition = %#v", first.RepresentativeEdition)
	}
	if results[1].ExternalID != "OL124W" || results[1].SourceRank != 2 || results[1].ArtworkURL != "" {
		t.Fatalf("second normalized result = %#v", results[1])
	}
}

func TestSearchPreferredMetadataLanguageSelectsEditionWithoutFilteringWorks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Has("lang") {
			t.Errorf("Search query contains lang=%q; preferred metadata language must not filter Works", request.URL.Query().Get("lang"))
		}
		_, _ = io.WriteString(w, `{"docs":[{"key":"/works/OL123W","title":"El ingenioso hidalgo Don Quijote","editions":{"docs":[{"key":"/books/OL321M","title":"Don Quijote","language":["jpn"]},{"key":"/books/OL322M","title":"Don Quijote","language":["spa"]}]}}]}`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, MetadataLanguage: "es"})
	results, err := source.Search(context.Background(), "Don Quijote", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].ExternalID != "OL123W" {
		t.Fatalf("Spanish metadata preference filtered a valid Work: %#v", results)
	}
	if results[0].RepresentativeEdition == nil || results[0].RepresentativeEdition.ExternalID != "OL322M" {
		t.Fatalf("representative edition = %#v, want Spanish edition OL322M", results[0].RepresentativeEdition)
	}
}

func TestSearchOriginalMetadataUsesProviderFirstEdition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Has("lang") {
			t.Errorf("Original metadata added a language filter: %q", request.URL.Query().Get("lang"))
		}
		_, _ = io.WriteString(w, `{"docs":[{"key":"/works/OL123W","title":"The Tale","editions":{"docs":[{"key":"/books/OL321M","title":"La historia","language":["spa"]},{"key":"/books/OL322M","title":"物語","language":["jpn"]}]}}]}`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, MetadataLanguage: "original"})
	results, err := source.Search(context.Background(), "The Tale", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].RepresentativeEdition == nil || results[0].RepresentativeEdition.ExternalID != "OL321M" {
		t.Fatalf("provider-default representative edition = %#v, want first edition OL321M", results)
	}
}

func TestSearchSkipsMalformedIndividualWorksAndOptionalFields(t *testing.T) {
	server := jsonServer(`{"docs":[{"key":"/works/OL1W","title":"  Standalone  "},{"key":"https://attacker.test/works/OL2W","title":"Unsafe identity"},{"key":"/works/OL3W","title":"  "},{"key":"/works/OL4W","title":"Invalid cover","cover_i":-1},{"key":"/works/OL5W","title":"Edition without cover","cover_i":17,"editions":{"docs":[{"key":"/books/OL5M","title":"Edition without cover","language":["eng"]}]}}]}`)
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	results, err := source.Search(context.Background(), "Standalone", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 3 || results[0].ExternalID != "OL1W" || results[0].Title != "Standalone" || results[0].Year != 0 || len(results[0].Creators) != 0 {
		t.Fatalf("optional/malformed results = %#v", results)
	}
	if results[1].ExternalID != "OL4W" || results[1].ArtworkURL != "" {
		t.Fatalf("invalid cover ID should be omitted: %#v", results[1])
	}
	if results[2].ExternalID != "OL5W" || results[2].RepresentativeEdition == nil || results[2].ArtworkURL != "https://covers.openlibrary.org/b/id/17-M.jpg" {
		t.Fatalf("missing representative cover should fall back to Work cover: %#v", results[2])
	}
}

func TestSearchBoundsQueryAndResponseBody(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"docs":[]}`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	if _, err := source.Search(context.Background(), strings.Repeat("x", 201), 1); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("oversized query error = %v, want invalid query", err)
	}
	if calls != 0 {
		t.Fatalf("requests for invalid query = %d, want 0", calls)
	}
	if _, err := source.Search(context.Background(), "title", 21); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("oversized result limit error = %v, want invalid query", err)
	}

	largeBody := strings.Repeat("x", MaxResponseBodyBytes+1)
	largeServer := jsonServer(largeBody)
	defer largeServer.Close()
	largeSource := New(ClientConfig{Endpoint: largeServer.URL, HTTPClient: largeServer.Client(), Limiter: immediateLimiter{}})
	if _, err := largeSource.Search(context.Background(), "title", 1); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized body error = %v, want response too large", err)
	}
}

func TestSearchRejectsMalformedPayloadAndReturnsSafeStatusErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
		want error
	}{
		{name: "malformed JSON", code: http.StatusOK, body: `{"docs":[`, want: ErrMalformedResponse},
		{name: "rate limited", code: http.StatusTooManyRequests, body: "private provider body", want: ErrSearchThrottled},
		{name: "provider failure", code: http.StatusInternalServerError, body: "private provider body", want: ErrSearchUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
			_, err := source.Search(context.Background(), "title", 1)
			if !errors.Is(err, test.want) {
				t.Fatalf("Search() error = %v, want %v", err, test.want)
			}
			if strings.Contains(fmt.Sprint(err), "private provider body") {
				t.Fatalf("provider response body leaked in error: %v", err)
			}
		})
	}
}

func TestSearchHonorsCancellationAndDoesNotFollowRedirects(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), RequestTimeout: time.Second, Limiter: immediateLimiter{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := source.Search(ctx, "title", 1)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Search() error = %v, want context.Canceled", err)
	}

	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls++
		_, _ = io.WriteString(w, `{"docs":[]}`)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	redirectSource := New(ClientConfig{Endpoint: redirect.URL, HTTPClient: redirect.Client(), Limiter: immediateLimiter{}})
	if _, err := redirectSource.Search(context.Background(), "title", 1); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("redirected Search() error = %v, want unavailable", err)
	}
	if destinationCalls != 0 {
		t.Fatalf("redirect destination calls = %d, want 0", destinationCalls)
	}
}

func TestRateLimiterSpacesRequestsAndHonorsCancellation(t *testing.T) {
	limiter := newRateLimiter(20 * time.Millisecond)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
		t.Fatalf("second rate-limited wait took %v, want at least 15ms", elapsed)
	}

	queued := newRateLimiter(time.Hour)
	if err := queued.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := queued.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled rate wait = %v, want context.DeadlineExceeded", err)
	}
}

func TestSearchUsesOneRequestPerSecondWithoutInjectedLimiter(t *testing.T) {
	source := New(ClientConfig{})
	limiter, ok := source.limiter.(*rateLimiter)
	if !ok || limiter.interval != time.Second {
		t.Fatalf("default limiter = %#v, want one request/second", source.limiter)
	}
}

func jsonServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
}
