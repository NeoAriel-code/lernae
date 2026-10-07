package tvmaze

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkDetailsLoadsExactShowAndNormalizesProviderMetadata(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		if request.Method != http.MethodGet {
			t.Errorf("request method = %s, want GET", request.Method)
			http.NotFound(w, request)
			return
		}
		switch request.URL.Path {
		case "/shows/42/seasons":
			_, _ = io.WriteString(w, `[{"id":1},{"id":2}]`)
		case "/shows/42":
			_, _ = io.WriteString(w, `{"id":42,"name":"The Wheel of Time","url":"https://www.tvmaze.com/shows/42/the-wheel-of-time","language":"English","status":"Running","genres":["Fantasy","Adventure"],"premiered":"2021-11-19","summary":"<p>A <em>turning</em> world &amp; more.</p><script>steal()</script>","image":{"original":"https://static.tvmaze.com/uploads/images/original_untouched/123/456789.jpg"},"network":{"name":"Prime Video"},"webChannel":{"name":"Amazon"}}`)
		default:
			t.Errorf("unexpected TVMaze Details request = %s %s", request.Method, request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	details, err := source.GetWorkDetails(context.Background(), "tvmaze", "42")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if strings.Join(paths, ",") != "/shows/42,/shows/42/seasons" {
		t.Fatalf("request paths = %v, want one show lookup followed by one seasons lookup", paths)
	}
	if details.Provider != "tvmaze" || details.ExternalID != "42" || details.Title != "The Wheel of Time" || details.Medium != "video" || details.WorkType != "series" {
		t.Fatalf("normalized show identity = %#v", details)
	}
	if details.Language != "en" || details.Status != "Running" || details.Network != "Prime Video" || details.WebChannel != "Amazon" ||
		len(details.Genres) != 2 || details.Genres[0] != "Fantasy" || details.ReleaseYear != 2021 {
		t.Fatalf("normalized show metadata = %#v", details)
	}
	if details.Summary != "A turning world & more." || strings.Contains(details.Summary, "<") || strings.Contains(details.Summary, "steal") {
		t.Fatalf("summary was not sanitized: %q", details.Summary)
	}
	if details.CoverReference != "https://static.tvmaze.com/uploads/images/original_untouched/123/456789.jpg" ||
		details.SourceURL != "https://www.tvmaze.com/shows/42/the-wheel-of-time" {
		t.Fatalf("validated artwork/source links = %#v", details)
	}
	if details.SeasonCount == nil || *details.SeasonCount != 2 {
		t.Fatalf("known season count = %v, want two seasons", details.SeasonCount)
	}
}

func TestWorkDetailsSanitizesUntrustedArtworkAndSourceLinks(t *testing.T) {
	server := jsonServer(`{"id":7,"name":"Unsafe links","url":"https://attacker.test/shows/7","image":{"original":"https://attacker.test/cover.jpg","medium":"http://static.tvmaze.com/uploads/images/medium_portrait/7.jpg"}}`)
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	details, err := source.GetWorkDetails(context.Background(), "tvmaze", "7")
	if err != nil {
		t.Fatalf("GetWorkDetails() error = %v", err)
	}
	if details.SourceURL != "https://www.tvmaze.com/shows/7" || details.CoverReference != "" {
		t.Fatalf("unsafe links were retained: %#v", details)
	}
}

func TestWorkDetailsRequiresExactShowIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		t.Error("invalid identity must not trigger a provider request")
		http.NotFound(w, request)
	}))
	defer server.Close()
	source := New(ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: immediateLimiter{}})
	for _, test := range []struct {
		provider string
		id       string
	}{
		{provider: "TVMaze", id: "42"},
		{provider: "tvmaze", id: "0"},
		{provider: "tvmaze", id: "42/other"},
		{provider: "tvmaze", id: "https://attacker.test/shows/42"},
	} {
		if _, err := source.GetWorkDetails(context.Background(), test.provider, test.id); err == nil {
			t.Errorf("GetWorkDetails(%q, %q) error = nil, want identity rejection", test.provider, test.id)
		}
	}
}
