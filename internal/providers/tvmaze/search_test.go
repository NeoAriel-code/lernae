package tvmaze

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type immediateLimiter struct{}

func (immediateLimiter) Wait(context.Context) error { return nil }

func TestSearchMapsRelevanceOrderAndSanitizesSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/search/shows" {
			t.Errorf("request = %s %s, want GET /search/shows", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("q") != "Wheel of Time" {
			t.Errorf("search query = %q", request.URL.Query().Get("q"))
		}
		_, _ = io.WriteString(w, `[{"score":0.99,"show":{"id":42,"name":"The Wheel of Time","url":"https://www.tvmaze.com/shows/42/the-wheel-of-time","type":"Scripted","premiered":"2021-11-19","genres":["Fantasy"],"summary":"<p>A <em>turning</em> world &amp; more.</p><script>steal()</script><style>.hidden{display:none}</style>","network":{"name":"Prime <em>Video</em>"},"webChannel":{"name":"Amazon"},"image":{"original":"https://static.tvmaze.com/uploads/images/original_untouched/123/456789.jpg","medium":"https://static.tvmaze.com/uploads/images/medium_portrait/123/456789.jpg"}}},{"score":0.5,"show":{"id":7,"name":"The Wheel of Time: Origins"}}]`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	results, err := source.Search(context.Background(), "Wheel of Time", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	first := results[0]
	if first.Provider != "tvmaze" || first.ExternalID != "42" || first.Title != "The Wheel of Time" || first.MediaType != "tv" || first.Medium != "video" || first.WorkType != "series" {
		t.Fatalf("normalized TV result = %#v", first)
	}
	if first.Year != 2021 || first.ReleaseYear != 2021 || first.Subtitle != "Scripted" || first.Summary != "A turning world & more." {
		t.Fatalf("normalized show metadata = %#v", first)
	}
	if first.Network != "Prime Video" || first.WebChannel != "Amazon" {
		t.Fatalf("normalized show channels = network %q, web channel %q; want Prime Video and Amazon", first.Network, first.WebChannel)
	}
	wantImage := "https://static.tvmaze.com/uploads/images/original_untouched/123/456789.jpg"
	if first.ArtworkURL != wantImage || first.CoverReference != wantImage || first.SourceURL != "https://www.tvmaze.com/shows/42/the-wheel-of-time" || first.SourceRank != 1 || first.Score != nil {
		t.Fatalf("show provenance/artwork = %#v", first)
	}
	if strings.Contains(first.Summary, "<") || strings.Contains(first.Summary, "steal") || strings.Contains(first.Summary, "hidden") || results[1].ExternalID != "7" || results[1].SourceRank != 2 {
		t.Fatalf("unsafe summary or unstable provider order: %#v", results)
	}
}

func TestSearchNeverFetchesSeasonSummary(t *testing.T) {
	var seasonsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/shows/42/seasons" {
			seasonsCalls.Add(1)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		if request.URL.Path != "/search/shows" {
			t.Errorf("Search() request path = %q, want only /search/shows", request.URL.Path)
		}
		_, _ = io.WriteString(w, `[{"show":{"id":42,"name":"Example"}}]`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	if _, err := source.Search(context.Background(), "Example", 5); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if seasonsCalls.Load() != 0 {
		t.Fatalf("Search() made %d TVMaze season requests, want zero", seasonsCalls.Load())
	}
}

func TestGetWorkDetailsAddsOneOptionalSeasonSummaryRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		seasonsBody string
		wantSeason  *int
	}{
		{name: "empty valid list reports zero known seasons", seasonsBody: `[]`, wantSeason: intPointer(0)},
		{name: "null optional list leaves season count unknown", seasonsBody: `null`},
		{name: "malformed optional list keeps valid Work Details", seasonsBody: `{"seasons":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var showCalls, seasonsCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/shows/42":
					showCalls.Add(1)
					_, _ = io.WriteString(w, `{"id":42,"name":"Example Show","type":"Scripted"}`)
				case "/shows/42/seasons":
					seasonsCalls.Add(1)
					_, _ = io.WriteString(w, test.seasonsBody)
				default:
					t.Errorf("unexpected TVMaze request path %q", request.URL.Path)
					http.NotFound(w, request)
				}
			}))
			defer server.Close()
			source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
			details, err := source.GetWorkDetails(context.Background(), "tvmaze", "42")
			if err != nil {
				t.Fatalf("GetWorkDetails() error = %v, want valid base details", err)
			}
			if showCalls.Load() != 1 || seasonsCalls.Load() != 1 {
				t.Fatalf("Details requests = show %d, seasons %d; want exactly one each", showCalls.Load(), seasonsCalls.Load())
			}
			if details.Title != "Example Show" || details.SeasonCount == nil != (test.wantSeason == nil) {
				t.Fatalf("base/optional Details = %#v, want season count %#v", details, test.wantSeason)
			}
			if test.wantSeason != nil && *details.SeasonCount != *test.wantSeason {
				t.Fatalf("season count = %d, want %d", *details.SeasonCount, *test.wantSeason)
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestSearchEmitsCanonicalTVMazeSourceRoutesAndRejectsUnsafeRoutes(t *testing.T) {
	for _, test := range []struct {
		name      string
		candidate string
		want      string
	}{
		{
			name:      "provider-normalized show URL",
			candidate: "https://www.tvmaze.com/shows/42/the-wheel-of-time",
			want:      "https://www.tvmaze.com/shows/42/the-wheel-of-time",
		},
		{
			name:      "untrusted host",
			candidate: "https://attacker.example/shows/42/the-wheel-of-time",
			want:      "https://www.tvmaze.com/shows/42",
		},
		{
			name:      "mismatched show identity",
			candidate: "https://www.tvmaze.com/shows/43/the-wheel-of-time",
			want:      "https://www.tvmaze.com/shows/42",
		},
		{
			name:      "wrong TVMaze path",
			candidate: "https://www.tvmaze.com/episodes/42/the-wheel-of-time",
			want:      "https://www.tvmaze.com/shows/42",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := normalizeShows([]searchHit{{Show: showResponse{
				ID: 42, Name: "The Wheel of Time", URL: test.candidate,
			}}}, 1)
			if len(results) != 1 || results[0].SourceURL != test.want {
				t.Fatalf("normalized route = %#v, want %s", results, test.want)
			}
		})
	}
}

func TestSearchValidatesArtworkHostAndHandlesMissingOptionalFields(t *testing.T) {
	server := jsonServer(`[{"show":{"id":1,"name":"Unsafe image","url":"https://attacker.test/shows/1","image":{"original":"https://attacker.test/poster.jpg","medium":"http://static.tvmaze.com/uploads/images/medium_portrait/poster.jpg"}}},{"show":{"id":2,"name":"Unsafe path","image":{"original":"https://static.tvmaze.com/uploads/images/../../elsewhere.jpg"}}},{"show":{"id":3,"name":"Safe fallback","image":{"original":"https://evil.test/poster.jpg","medium":"https://static.tvmaze.com/uploads/images/medium_portrait/safe.jpg"}}}]`)
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	results, err := source.Search(context.Background(), "title", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("result count = %d, want 3", len(results))
	}
	if results[0].ArtworkURL != "" || results[0].Year != 0 || results[0].Summary != "" || len(results[0].Creators) != 0 || results[0].SourceURL != "https://www.tvmaze.com/shows/1" {
		t.Fatalf("missing optional fields should be omitted: %#v", results[0])
	}
	if results[0].Network != "" || results[0].WebChannel != "" {
		t.Fatalf("missing optional channel fields should be empty: %#v", results[0])
	}
	if results[1].ArtworkURL != "" || results[2].ArtworkURL != "https://static.tvmaze.com/uploads/images/medium_portrait/safe.jpg" {
		t.Fatalf("unsafe or fallback artwork mapping = %#v", results)
	}
}

func TestSearchBoundsQueryBodyAndResultsAndMapsSafeErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `[{"show":{"id":1,"name":"One"}},{"show":{"id":2,"name":"Two"}}]`)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	if _, err := source.Search(context.Background(), strings.Repeat("x", 201), 1); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("oversized query error = %v, want invalid query", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid query request count = %d, want 0", calls.Load())
	}
	results, err := source.Search(context.Background(), "title", 1)
	if err != nil || len(results) != 1 {
		t.Fatalf("result bound = %#v, %v, want one result", results, err)
	}

	large := jsonServer(strings.Repeat("x", MaxResponseBodyBytes+1))
	defer large.Close()
	largeSource := New(ClientConfig{Endpoint: large.URL, HTTPClient: large.Client(), Limiter: immediateLimiter{}})
	if _, err := largeSource.Search(context.Background(), "title", 1); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized body error = %v, want response too large", err)
	}

	for _, test := range []struct {
		name string
		code int
		want error
	}{
		{name: "malformed JSON", code: http.StatusOK, want: ErrMalformedResponse},
		{name: "server failure", code: http.StatusInternalServerError, want: ErrSearchUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, `{"private":"provider response"`)
			}))
			defer failure.Close()
			failureSource := New(ClientConfig{Endpoint: failure.URL, HTTPClient: failure.Client(), Limiter: immediateLimiter{}})
			_, err := failureSource.Search(context.Background(), "title", 1)
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "provider response") {
				t.Fatalf("safe error = %v, want %v with no provider detail", err, test.want)
			}
		})
	}
}

func TestSearchRetries429OnceAfterBoundedBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `[{"show":{"id":1,"name":"Retry result"}}]`)
	}))
	defer server.Close()
	backoff := 15 * time.Millisecond
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, RetryBackoff: backoff})
	started := time.Now()
	results, err := source.Search(context.Background(), "title", 1)
	if err != nil || len(results) != 1 || results[0].Title != "Retry result" || calls.Load() != 2 {
		t.Fatalf("retry result/calls/error = %#v/%d/%v", results, calls.Load(), err)
	}
	if time.Since(started) < backoff {
		t.Fatalf("429 retry returned before configured backoff %v", backoff)
	}

	var throttledCalls atomic.Int32
	throttled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		throttledCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer throttled.Close()
	throttledSource := New(ClientConfig{Endpoint: throttled.URL, HTTPClient: throttled.Client(), Limiter: immediateLimiter{}, RetryBackoff: time.Millisecond})
	if _, err := throttledSource.Search(context.Background(), "title", 1); !errors.Is(err, ErrSearchThrottled) || throttledCalls.Load() != 2 {
		t.Fatalf("repeated 429 error/calls = %v/%d, want throttled after exactly one retry", err, throttledCalls.Load())
	}
}

func TestSearchCancelsHardHungHTTPRequestAtProviderDeadline(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	defer server.Close()

	source := New(ClientConfig{
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		RequestTimeout: 40 * time.Millisecond,
		Limiter:        immediateLimiter{},
	})
	results, err := source.Search(context.Background(), "stalled request", 1)
	if !errors.Is(err, context.DeadlineExceeded) || len(results) != 0 {
		t.Fatalf("stalled request result/error = %#v/%v, want no results and provider deadline", results, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fake server never received the search request")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("provider deadline did not cancel the hard-hung HTTP request")
	}
}

func TestSearchCancellationInterruptsBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}, RetryBackoff: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := source.Search(ctx, "title", 1)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("canceled retry error/calls = %v/%d, want deadline after one call", err, calls.Load())
	}
}

func TestSearchDefaultsToDocumentedTVMazeRequestPacing(t *testing.T) {
	source := New(ClientConfig{})
	limiter, ok := source.limiter.(*rateLimiter)
	if !ok || limiter.interval != 500*time.Millisecond {
		t.Fatalf("default limiter = %#v, want one request per 500ms", source.limiter)
	}
}

func jsonServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
}
